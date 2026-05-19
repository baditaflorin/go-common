// Package apikey is the canonical client for the fleet's keystore service
// (baditaflorin/go-apikey-service). Every fleet component that needs to
// verify, issue, or list keys should import this — never reimplement.
//
// The keystore is the security crown jewel: if it falls, every 0exec
// service falls. This package enforces three discipline:
//
//   1. A single HTTP client with timeouts (no caller-set unbounded retries).
//   2. Constant-time admin-token header construction (mirrors server side).
//   3. A LocalCache layer that absorbs short keystore outages without
//      authenticating new keys — see Cache.Verify and Verifier interface.
//
// Wire format
//
//   POST /verify   headers: X-Verify-Key=<key>
//                  200  → valid (headers X-Auth-User, X-Auth-Scope)
//                  401  → invalid / expired / revoked
//                  5xx  → keystore problem; caller chooses degrade policy
//
//   POST /issue    headers: X-Admin-Token=<admin>; JSON body
//   POST /revoke   headers: X-Admin-Token=<admin>; JSON body {"key":"..."}
//   GET  /list     headers: X-Admin-Token=<admin>; returns {"keys":[...]}
//   POST /purge    headers: X-Admin-Token=<admin>; no body
//
// Environment
//
//   APIKEY_SERVICE_URL          base URL (default: http://localhost:18021)
//   APIKEY_SERVICE_ADMIN_TOKEN  the admin token; required for admin ops
//
// Production URL + admin token live in private fleet-state/OPS.md.
package apikey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Client is a minimal, timeout-bounded HTTP client for the keystore.
// Construct one per process and reuse — don't churn HTTPClient instances.
type Client struct {
	BaseURL    string        // default from APIKEY_SERVICE_URL
	AdminToken string        // default from APIKEY_SERVICE_ADMIN_TOKEN
	HTTPClient *http.Client  // default: 5s timeout, no redirects
	UserAgent  string        // default: "go-common/apikey"

	// AdminObs (optional) receives one AdminEvent per Issue / Revoke /
	// List / Purge call. promx.NewAdminCollectors returns an
	// implementation that records fleet-canonical Prometheus metrics.
	AdminObs AdminObserver
}

// New returns a Client wired from env vars. AdminToken may be empty if
// the caller only does /verify (no admin endpoints).
func New() *Client {
	return &Client{
		BaseURL:    envOr("APIKEY_SERVICE_URL", "http://localhost:18021"),
		AdminToken: os.Getenv("APIKEY_SERVICE_ADMIN_TOKEN"),
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		UserAgent: "go-common/apikey",
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

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

// Verify checks the key against the keystore. Returns ErrInvalidKey for
// definitive rejections (401), ErrKeystoreUnavailable for transient
// failures the caller may want to degrade through.
func (c *Client) Verify(ctx context.Context, key string) (*VerifyResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/verify", nil)
	if err != nil {
		return nil, fmt.Errorf("apikey verify: build req: %w", err)
	}
	req.Header.Set("X-Verify-Key", key)
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeystoreUnavailable, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return &VerifyResult{
			User:  resp.Header.Get("X-Auth-User"),
			Scope: resp.Header.Get("X-Auth-Scope"),
		}, nil
	case http.StatusUnauthorized:
		return nil, ErrInvalidKey
	default:
		return nil, fmt.Errorf("%w: HTTP %d", ErrKeystoreUnavailable, resp.StatusCode)
	}
}

// ─── Admin endpoints ───────────────────────────────────────────────────

// IssueRequest mirrors the server's issueReq.
type IssueRequest struct {
	User         string `json:"user"`
	TTLSeconds   int64  `json:"ttl_seconds,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Note         string `json:"note,omitempty"`
	NeverExpires bool   `json:"never_expires,omitempty"`
	Key          string `json:"key,omitempty"` // migration only
}

// IssueResult is what /issue returns.
type IssueResult struct {
	Key       string `json:"key"`
	User      string `json:"user"`
	Scope     string `json:"scope"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

// Issue mints a new key. Requires AdminToken.
func (c *Client) Issue(ctx context.Context, req IssueRequest) (*IssueResult, error) {
	if c.AdminToken == "" {
		return nil, ErrAdminTokenMissing
	}
	body, _ := json.Marshal(req)
	out := &IssueResult{}
	if err := c.adminCall(ctx, http.MethodPost, "/issue", body, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Revoke marks a key revoked. Returns whether anything was actually
// revoked (false if already revoked or unknown).
func (c *Client) Revoke(ctx context.Context, key string) (bool, error) {
	if c.AdminToken == "" {
		return false, ErrAdminTokenMissing
	}
	body, _ := json.Marshal(map[string]string{"key": key})
	var out struct {
		Revoked bool `json:"revoked"`
	}
	if err := c.adminCall(ctx, http.MethodPost, "/revoke", body, &out); err != nil {
		return false, err
	}
	return out.Revoked, nil
}

// KeyMeta is one row returned by /list.
type KeyMeta struct {
	Key        string `json:"key"`
	User       string `json:"user"`
	Scope      string `json:"scope"`
	Note       string `json:"note"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	Expired    bool   `json:"expired"`
}

// List returns all known keys (capped at 500 by the server).
func (c *Client) List(ctx context.Context) ([]KeyMeta, error) {
	if c.AdminToken == "" {
		return nil, ErrAdminTokenMissing
	}
	var out struct {
		Keys []KeyMeta `json:"keys"`
	}
	if err := c.adminCall(ctx, http.MethodGet, "/list", nil, &out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}

// Purge drops expired/revoked rows. Returns how many were deleted.
func (c *Client) Purge(ctx context.Context) (int, error) {
	if c.AdminToken == "" {
		return 0, ErrAdminTokenMissing
	}
	var out struct {
		Purged int `json:"purged"`
	}
	if err := c.adminCall(ctx, http.MethodPost, "/purge", nil, &out); err != nil {
		return 0, err
	}
	return out.Purged, nil
}

func (c *Client) adminCall(ctx context.Context, method, path string, body []byte, out any) (retErr error) {
	start := time.Now()
	result := "ok"
	defer func() {
		if c.AdminObs != nil {
			c.AdminObs.ObserveAdmin(AdminEvent{
				Op:       adminOpFromPath(path),
				Result:   result,
				Duration: time.Since(start),
			})
		}
	}()

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		result = "client_error"
		return fmt.Errorf("apikey %s %s: %w", method, path, err)
	}
	req.Header.Set("X-Admin-Token", c.AdminToken)
	req.Header.Set("User-Agent", c.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		result = "transport_error"
		return fmt.Errorf("%w: %v", ErrKeystoreUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		result = "unavailable"
		return fmt.Errorf("%w: HTTP %d", ErrKeystoreUnavailable, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		result = "unauthorized"
		return errors.New("apikey: admin unauthorized (check APIKEY_SERVICE_ADMIN_TOKEN)")
	}
	if resp.StatusCode >= 400 {
		result = "client_error"
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("apikey %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
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

// Cache wraps a Verifier with a positive-result-only cache for graceful
// degradation when the keystore goes down. Negative results (ErrInvalidKey)
// are NEVER cached — we want a definitive 401 the moment a key is revoked.
//
// When the wrapped Verifier returns ErrKeystoreUnavailable, Cache will
// serve a previously-cached positive result if its age is under StaleTTL.
//
// Rationale: a brief keystore blip shouldn't disconnect every authenticated
// caller. But cached results are short-lived enough that revoked keys
// don't keep working forever.
type Cache struct {
	Inner    Verifier
	FreshTTL time.Duration // how long a cached hit counts as "fresh" (no upstream call)
	StaleTTL time.Duration // how long a cached hit can fall back to during upstream outage

	// Observer (optional) receives one CacheEvent per Verify call.
	// promx.NewAuthCollectors() returns an implementation that records
	// fleet-canonical Prometheus metrics (cache hit rate, stale-serve
	// rate during outages, upstream call latency).
	Observer CacheObserver

	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	result   VerifyResult
	verified time.Time
}

// NewCache returns a Cache wrapping inner with sensible defaults
// (FreshTTL=60s, StaleTTL=15m).
func NewCache(inner Verifier) *Cache {
	return &Cache{
		Inner:    inner,
		FreshTTL: 60 * time.Second,
		StaleTTL: 15 * time.Minute,
		entries:  map[string]cacheEntry{},
	}
}

// Verify implements the Verifier interface with cache logic:
//   - cache hit < FreshTTL: return cached, no upstream call.
//   - otherwise: call upstream.
//   - upstream OK: cache + return.
//   - upstream ErrInvalidKey: clear cache + propagate (definitive 401).
//   - upstream ErrKeystoreUnavailable: if cache hit < StaleTTL, return
//     cached + log; otherwise propagate ErrKeystoreUnavailable.
func (c *Cache) Verify(ctx context.Context, key string) (*VerifyResult, error) {
	now := time.Now()
	c.mu.RLock()
	entry, hadEntry := c.entries[key]
	c.mu.RUnlock()
	if hadEntry && now.Sub(entry.verified) < c.FreshTTL {
		r := entry.result
		c.observe(CacheEvent{Result: CacheResultFresh, Age: now.Sub(entry.verified)})
		return &r, nil
	}

	start := time.Now()
	res, err := c.Inner.Verify(ctx, key)
	dur := time.Since(start)
	if err == nil {
		c.mu.Lock()
		c.entries[key] = cacheEntry{result: *res, verified: now}
		c.mu.Unlock()
		c.observe(CacheEvent{Result: CacheResultInnerOK, Duration: dur})
		return res, nil
	}
	if errors.Is(err, ErrInvalidKey) {
		// Definitive rejection — drop any cached entry for this key.
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		c.observe(CacheEvent{Result: CacheResultInnerInvalid, Duration: dur})
		return nil, err
	}
	// Upstream unavailable — see if we have a stale-but-tolerable cache.
	if hadEntry && now.Sub(entry.verified) < c.StaleTTL {
		r := entry.result
		c.observe(CacheEvent{Result: CacheResultStale, Age: now.Sub(entry.verified), Duration: dur})
		return &r, nil
	}
	c.observe(CacheEvent{Result: CacheResultInnerUnavailable, Duration: dur})
	return nil, err
}

func (c *Cache) observe(ev CacheEvent) {
	if c.Observer != nil {
		c.Observer.ObserveCache(ev)
	}
}

// Snapshot returns a copy of the current cache for observability /
// metrics endpoints. Don't use for auth decisions.
func (c *Cache) Snapshot() map[string]time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]time.Time, len(c.entries))
	for k, e := range c.entries {
		out[k] = e.verified
	}
	return out
}
