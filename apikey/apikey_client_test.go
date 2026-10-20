package apikey

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/header"
)

// TestClient_Verify_DecodesTierHeader proves Client.Verify reads the new
// X-Auth-Tier response header into VerifyResult.Tier, alongside the
// existing User/Scope headers — additive, backward-compatible wire format.
func TestClient_Verify_DecodesTierHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(header.AuthUser, "alice")
		w.Header().Set(header.AuthScope, "*")
		w.Header().Set(header.AuthTier, "vetted-pentest")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client(), UserAgent: "test"}
	res, err := c.Verify(context.Background(), "ak_test")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.User != "alice" || res.Scope != "*" {
		t.Fatalf("unexpected user/scope: %+v", res)
	}
	if res.Tier != "vetted-pentest" {
		t.Errorf("Tier = %q, want %q", res.Tier, "vetted-pentest")
	}
}

// TestClient_Verify_MissingTierHeader_IsEmptyNotError proves a key issued
// before tiering existed (no X-Auth-Tier on the response) verifies fine
// with an empty Tier, not an error — callers must treat "" as lowest
// trust, never as "all tiers granted".
func TestClient_Verify_MissingTierHeader_IsEmptyNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(header.AuthUser, "bob")
		w.Header().Set(header.AuthScope, "*")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client(), UserAgent: "test"}
	res, err := c.Verify(context.Background(), "ak_test")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Tier != "" {
		t.Errorf("Tier = %q, want empty for a pre-tiering key", res.Tier)
	}
}

// An error response from the keystore ends up in the returned error, which ends up
// in logs: only its first 4 KiB do.
func TestClient_AdminErrorBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(bytes.Repeat([]byte("e"), 1<<20))
	}))
	defer srv.Close()
	c := &Client{Endpoints: []string{srv.URL}, HTTPClient: srv.Client(), UserAgent: "test", AdminToken: "t"}
	err := c.adminCall(context.Background(), http.MethodPost, "/admin/revoke", nil, nil)
	if err == nil {
		t.Fatal("want an error for a 400")
	}
	if n := len(err.Error()); n > 4<<10+200 {
		t.Fatalf("error message is %d bytes; the body must be cut at 4 KiB", n)
	}
	if !strings.HasSuffix(err.Error(), "[truncated]") {
		t.Fatalf("a cut body is marked: %q", err.Error()[len(err.Error())-30:])
	}

	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(" no such key \n"))
	}))
	defer short.Close()
	c.Endpoints, c.HTTPClient = []string{short.URL}, short.Client()
	err = c.adminCall(context.Background(), http.MethodPost, "/admin/revoke", nil, nil)
	if err == nil || !strings.HasSuffix(err.Error(), "HTTP 400: no such key") {
		t.Fatalf("a short error body is unchanged: %v", err)
	}
}
