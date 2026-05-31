package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/response"
)

func newVault(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-key", srv.Client())
}

func TestGet_WrappedEnvelope(t *testing.T) {
	c := newVault(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/secrets/hcloud_token" {
			w.WriteHeader(404)
			return
		}
		_ = writeJSON(w, response.Success(map[string]any{
			"name":  "hcloud_token",
			"value": "tok-123",
		}))
	})
	got, err := c.Get(context.Background(), "hcloud_token")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "tok-123" {
		t.Fatalf("value = %q want tok-123", got)
	}
}

func TestGet_ErrorEnvelopeSurfaces(t *testing.T) {
	c := newVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_ = writeJSON(w, response.NewError(403, "auth.scope_mismatch", "not in consumers"))
	})
	_, err := c.Get(context.Background(), "hcloud_token")
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("want status 403 in error, got %v", err)
	}
}

func TestGet_EmptyValueRejected(t *testing.T) {
	c := newVault(t, func(w http.ResponseWriter, r *http.Request) {
		_ = writeJSON(w, response.Success(map[string]any{"name": "x", "value": ""}))
	})
	_, err := c.Get(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-value error, got %v", err)
	}
}

func TestGet_NeverLeaksValueInError(t *testing.T) {
	// 200 but the data is a JSON string, not an object → decode fails.
	// The error must not contain the secret-looking payload.
	c := newVault(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":"leaky-secret-xyz"}`))
	})
	_, err := c.Get(context.Background(), "x")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if strings.Contains(err.Error(), "leaky-secret-xyz") {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestGet_Misconfigured(t *testing.T) {
	if _, err := (*Client)(nil).Get(context.Background(), "x"); err == nil {
		t.Fatal("nil client should error")
	}
	if _, err := New("", "k", http.DefaultClient).Get(context.Background(), "x"); err == nil {
		t.Fatal("empty base URL should error")
	}
}

// writeJSON mirrors the tiny helper services use to emit an envelope.
func writeJSON(w http.ResponseWriter, body any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(body)
}
