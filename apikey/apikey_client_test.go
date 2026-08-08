package apikey

import (
	"context"
	"net/http"
	"net/http/httptest"
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
