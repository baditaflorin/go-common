package apikey

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/baditaflorin/go-common/secrets"
)

// DefaultAdminTokenSecretName is the go-fleet-secrets entry name
// ResolveAdminToken reads its live value from. Fixed rather than
// caller-configurable: one canonical secret name means "which vault
// entry backs the keystore admin token" is answerable by grepping this
// constant, not tracing which string literal a given service happened
// to pass in.
const DefaultAdminTokenSecretName = "apikey-service-admin-token"

// ResolveAdminToken returns the apikey-service admin token a Client
// needs for Issue/Revoke/List/Purge, preferring a live read from
// go-fleet-secrets (DefaultAdminTokenSecretName) and falling back to
// the static APIKEY_SERVICE_ADMIN_TOKEN env var — the same value
// Client.AdminToken defaults to via New() — when the vault is
// unreachable, the caller has no vault credentials configured, or the
// secret read fails for any reason.
//
// Why this exists: the admin token has no other canonical source of
// truth today. Every admin-op consumer copies the current value into
// its own .env at provision time; nothing re-syncs it on rotation, so
// consumers silently drift stale (confirmed live 2026-09-23: three
// fleet services carried a value that no longer matched the keystore's
// actual ADMIN_TOKEN after one rotation, and the self-service key
// portal — the one consumer that actually calls Issue — failed 401 for
// every caller until traced by hand). Centralising a vault read here
// means rotating the ONE go-fleet-secrets entry is enough going
// forward — any consumer that adopts this picks up the current value
// on its next restart without a manual .env edit.
//
// vaultBaseURL and vaultAPIKey are typically FLEET_SECRETS_URL and
// FLEET_API_KEY from the caller's own env — passed explicitly rather
// than read internally so a caller with neither configured (the common
// case: most apikey.Client consumers only ever call Verify, never an
// admin op, and have no reason to hold vault credentials) doesn't pay
// for a vault round-trip it never asked for. Passing either as "" skips
// the vault entirely and returns the env fallback immediately — no
// network call, no context deadline to reason about.
//
// This performs at most one HTTP call, bounded by a 3s internal
// timeout independent of ctx's own deadline, so a slow or down vault
// can never block a caller's startup longer than that. Never returns
// an error: a resolution failure is not fatal here, it just means "use
// whatever the env fallback already had" — exactly today's behavior.
func ResolveAdminToken(ctx context.Context, vaultBaseURL, vaultAPIKey string) string {
	envFallback := os.Getenv("APIKEY_SERVICE_ADMIN_TOKEN")
	if vaultBaseURL == "" || vaultAPIKey == "" {
		return envFallback
	}

	vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	client := secrets.New(vaultBaseURL, vaultAPIKey, &http.Client{Timeout: 3 * time.Second})
	val, err := client.Get(vctx, DefaultAdminTokenSecretName)
	if err != nil || val == "" {
		return envFallback
	}
	return val
}
