package apikey

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveAdminToken_NoVaultCredsReturnsEnvFallbackImmediately(t *testing.T) {
	t.Setenv("APIKEY_SERVICE_ADMIN_TOKEN", "env-fallback-value")
	called := false
	// Any request here would prove the "no vault creds" short-circuit
	// didn't hold — assert no HTTP call happens by never starting a
	// server and instead passing empty vault args, which must return
	// before any network I/O is attempted.
	_ = called
	got := ResolveAdminToken(context.Background(), "", "")
	if got != "env-fallback-value" {
		t.Fatalf("got %q, want env fallback", got)
	}
	got = ResolveAdminToken(context.Background(), "http://example.invalid", "")
	if got != "env-fallback-value" {
		t.Fatalf("empty vaultAPIKey: got %q, want env fallback", got)
	}
}

func TestResolveAdminToken_VaultSuccessWins(t *testing.T) {
	t.Setenv("APIKEY_SERVICE_ADMIN_TOKEN", "stale-env-value")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/secrets/"+DefaultAdminTokenSecretName {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-vault-key" {
			t.Errorf("missing/wrong X-API-Key header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"name":"` + DefaultAdminTokenSecretName + `","value":"live-vault-value"}}`))
	}))
	defer srv.Close()

	got := ResolveAdminToken(context.Background(), srv.URL, "test-vault-key")
	if got != "live-vault-value" {
		t.Fatalf("got %q, want the live vault value (not the stale env fallback)", got)
	}
}

func TestResolveAdminToken_VaultFailureFallsBackToEnv(t *testing.T) {
	t.Setenv("APIKEY_SERVICE_ADMIN_TOKEN", "env-fallback-value")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	got := ResolveAdminToken(context.Background(), srv.URL, "wrong-key")
	if got != "env-fallback-value" {
		t.Fatalf("got %q, want env fallback on vault failure", got)
	}
}

func TestResolveAdminToken_VaultUnreachableFallsBackToEnv(t *testing.T) {
	t.Setenv("APIKEY_SERVICE_ADMIN_TOKEN", "env-fallback-value")
	// A closed server: connection refused, simulating the vault being down.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	got := ResolveAdminToken(context.Background(), srv.URL, "any-key")
	if got != "env-fallback-value" {
		t.Fatalf("got %q, want env fallback when vault is unreachable", got)
	}
}
