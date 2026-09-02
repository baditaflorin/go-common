package apikey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// successEnvelope mimics the response.Success wire shape from go-apikey-service.
func successEnvelope(data any) any {
	return map[string]any{"status": "success", "data": data}
}

// TestAdminCall_UnwrapsSuccessEnvelope proves that adminCall correctly
// decodes the {"status":"success","data":{...}} wrapper that every
// go-apikey-service admin endpoint returns.
//
// Regression test for the silent zero-value bug: before the fix,
// json.Decode saw {"status","data"} at the top level and silently
// produced zero-value output for every admin struct.
func TestAdminCall_UnwrapsSuccessEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Admin-Token") == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(successEnvelope(map[string]any{
			"key":        "ak_testkey1234",
			"user":       "alice",
			"scope":      "alice",
			"note":       "test note",
			"created_at": "2026-05-26T00:00:00Z",
			"expires_at": "2026-06-26T00:00:00Z",
		}))
	}))
	defer srv.Close()

	c := &Client{
		BaseURL:    srv.URL,
		AdminToken: "test-admin-token",
		HTTPClient: srv.Client(),
		UserAgent:  "test",
	}

	res, err := c.Issue(context.Background(), IssueRequest{
		User:       "alice",
		TTLSeconds: 30 * 24 * 3600,
		Scope:      "alice",
		Note:       "test note",
	})
	if err != nil {
		t.Fatalf("Issue: unexpected error: %v", err)
	}
	if res.Key != "ak_testkey1234" {
		t.Errorf("Key: got %q, want %q", res.Key, "ak_testkey1234")
	}
	if res.User != "alice" {
		t.Errorf("User: got %q, want %q", res.User, "alice")
	}
	if res.Scope != "alice" {
		t.Errorf("Scope: got %q, want %q", res.Scope, "alice")
	}
	if res.CreatedAt == "" {
		t.Error("CreatedAt: got empty, want non-empty")
	}
	if res.ExpiresAt == "" {
		t.Error("ExpiresAt: got empty, want non-empty")
	}
}

// TestAdminCall_Revoke_UnwrapsEnvelope confirms that Revoke correctly reads
// "revoked" from inside the "data" envelope.
func TestAdminCall_Revoke_UnwrapsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(successEnvelope(map[string]any{"revoked": true}))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, AdminToken: "tok", HTTPClient: srv.Client()}
	revoked, err := c.Revoke(context.Background(), "ak_somekey")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !revoked {
		t.Error("Revoke: got false, want true")
	}
}

// TestAdminCall_List_UnwrapsEnvelope confirms that List correctly reads
// the "keys" array from inside the "data" envelope.
func TestAdminCall_List_UnwrapsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(successEnvelope(map[string]any{
			"keys": []map[string]any{
				{"key": "ak_aaa", "user": "bob", "scope": "*"},
			},
		}))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, AdminToken: "tok", HTTPClient: srv.Client()}
	keys, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("List: got %d keys, want 1", len(keys))
	}
	if keys[0].Key != "ak_aaa" {
		t.Errorf("List[0].Key: got %q, want %q", keys[0].Key, "ak_aaa")
	}
}

func TestAdminCall_ListExact_UnwrapsEnvelopeAndUsesAllIdentityFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/list" {
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
			return
		}
		if got, want := r.URL.Query().Get("user"), "graph-writer"; got != want {
			http.Error(w, "wrong user selector", http.StatusBadRequest)
			return
		}
		if got, want := r.URL.Query().Get("scope"), "fleet-graph.0exec.com"; got != want {
			http.Error(w, "wrong scope selector", http.StatusBadRequest)
			return
		}
		if got, want := r.URL.Query().Get("tier"), "fleet-graph-writer"; got != want {
			http.Error(w, "wrong tier selector", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(successEnvelope(map[string]any{
			"keys":      []map[string]any{{"key": "ak_aaa", "user": "graph-writer", "scope": "fleet-graph.0exec.com", "tier": "fleet-graph-writer"}},
			"truncated": true,
		}))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, AdminToken: "tok", HTTPClient: srv.Client()}
	result, err := c.ListExact(context.Background(), "graph-writer", "fleet-graph.0exec.com", "fleet-graph-writer")
	if err != nil {
		t.Fatalf("ListExact: %v", err)
	}
	if len(result.Keys) != 1 || result.Keys[0].Key != "ak_aaa" || !result.Truncated {
		t.Fatalf("ListExact result = %+v", result)
	}
}

func TestAdminCall_ListExactRejectsPartialIdentity(t *testing.T) {
	c := &Client{AdminToken: "tok"}
	if _, err := c.ListExact(context.Background(), "graph-writer", "", "fleet-graph-writer"); err == nil {
		t.Fatal("ListExact accepted a partial identity")
	}
}
