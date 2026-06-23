// Package apikey is the canonical client for the fleet's keystore service
// (baditaflorin/go-apikey-service). Every fleet component that needs to
// verify, issue, or list keys should import this — never reimplement.
//
// The keystore is the security crown jewel: if it falls, every 0exec
// service falls. This package enforces three discipline:
//
//  1. A single HTTP client with timeouts (no caller-set unbounded retries).
//  2. Constant-time admin-token header construction (mirrors server side).
//  3. A LocalCache layer that absorbs short keystore outages without
//     authenticating new keys — see Cache.Verify and Verifier interface.
//
// Wire format
//
//	POST /verify   headers: X-Verify-Key=<key>
//	               200  → valid (headers X-Auth-User, X-Auth-Scope)
//	               401  → invalid / expired / revoked
//	               5xx  → keystore problem; caller chooses degrade policy
//
//	POST /issue    headers: X-Admin-Token=<admin>; JSON body
//	POST /revoke   headers: X-Admin-Token=<admin>; JSON body {"key":"..."}
//	GET  /list     headers: X-Admin-Token=<admin>; returns {"keys":[...]}
//	POST /purge    headers: X-Admin-Token=<admin>; no body
//
// Environment
//
//	APIKEY_SERVICE_URL          base URL (default: http://localhost:18021)
//	APIKEY_SERVICE_ADMIN_TOKEN  the admin token; required for admin ops
//
// Production URL + admin token live in private fleet-state/OPS.md.
package apikey

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ─── Errors ────────────────────────────────────────────────────────────

// ErrInvalidKey means /verify returned 401 — the key is unknown, expired,
// or revoked. Callers should NOT retry; the answer is definitive.
var ErrInvalidKey = errors.New("apikey: invalid")

// ErrKeystoreUnavailable means /verify returned 5xx or the network
// failed. Callers should degrade gracefully (see Cache).
var ErrKeystoreUnavailable = errors.New("apikey: keystore unavailable")

// ErrAdminTokenMissing means the caller invoked an admin endpoint
// without setting APIKEY_SERVICE_ADMIN_TOKEN.
var ErrAdminTokenMissing = errors.New("apikey: admin token not configured")

// ─── /verify ───────────────────────────────────────────────────────────

// VerifyResult captures what /verify returns when valid.
type VerifyResult struct {
	User  string
	Scope string
}

// ─── Admin endpoints ───────────────────────────────────────────────────

// KeyMeta is one row returned by /list.
type KeyMeta struct {
	Key        string `json:"key"`
	User       string `json:"user"`
	Scope      string `json:"scope"`
	Name       string `json:"name,omitempty"`
	Note       string `json:"note"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	Expired    bool   `json:"expired"`
	UseCount   int64  `json:"use_count"`
	UseLimit   *int64 `json:"use_limit,omitempty"` // nil = unlimited
}

// adminOpFromPath maps the admin URL path to a stable, low-cardinality
// op label. Unknown paths fold to "_other" so a future endpoint
// doesn't blow up cardinality before its own bucket is added.
func adminOpFromPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/issue"):
		return "issue"
	case strings.HasPrefix(path, "/revoke"):
		return "revoke"
	case strings.HasPrefix(path, "/list"):
		return "list"
	case strings.HasPrefix(path, "/purge"):
		return "purge"
	default:
		return "_other"
	}
}

// ─── Graceful degradation ──────────────────────────────────────────────

// Verifier is the abstract interface anything that wants to verify keys
// should depend on. Lets tests stub and lets Cache wrap Client.
type Verifier interface {
	Verify(ctx context.Context, key string) (*VerifyResult, error)
}

type cacheEntry struct {
	result   VerifyResult
	verified time.Time
}
