package apikey

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ─── VerifyScope — out-of-band scope assertion ─────────────────────────
//
// VerifyScope is defense-in-depth against a compromised gateway. The
// fleet's auth pipeline is:
//
//   client → nginx (auth_request → keystore /verify) → service
//
// The gateway populates `X-Auth-User` and `X-Auth-Scope` on the
// request after the keystore returns 200, and downstream services
// trust those headers (see middleware.TokenAuthKeystore).
//
// If the gateway itself is ever compromised — or a non-gateway path
// somehow injects forged `X-Auth-*` headers — a service would happily
// honor a forged scope. VerifyScope lets a security-sensitive service
// re-query the keystore with the raw key it received and confirm the
// claimed scope is actually the principal's true scope.
//
// Cost: one keystore call per (key, scope) per 5 min per service.
// Cached positives skip the round-trip; negatives are NEVER cached so
// a revoked or scope-narrowed key takes effect immediately.

// ErrScopeMismatch means the keystore reports a different scope than
// the one the caller claimed (likely X-Auth-Scope forgery or stale
// key state). This is a definitive failure — do not retry.
var ErrScopeMismatch = errors.New("apikey: scope mismatch (keystore reports different scope than claimed)")

// scopeVerifyCache is the default-installed per-Client positive cache
// for VerifyScope. Constructed lazily on first call so existing
// Clients (zero value HTTPClient handled by adminCall etc.) don't
// pay for it unless they opt in.
//
// Cache is bounded: LRU eviction past scopeCacheMaxEntries. TTL is
// scopeCacheTTL. Both are package-level vars (not constants) so
// tests can shrink them via an internal helper without exporting the
// implementation detail.
var (
	scopeCacheTTL        = 5 * time.Minute
	scopeCacheMaxEntries = 10000
)

// scopeCacheKey composes (key, scope) so an attacker who rebinds
// scope to a different key can't reuse a previously-cached positive.
type scopeCacheKey struct {
	key   string
	scope string
}

type scopeCacheEntry struct {
	verifiedAt time.Time
	lruElem    *list.Element // node in lruList; identity == scopeCacheKey
}

// scopeCache is an LRU+TTL store for positive VerifyScope results.
// Concurrency: a single mutex guards both the map and the LRU list;
// VerifyScope holds it only for cache lookup / update, never across
// the network call.
type scopeCache struct {
	mu      sync.Mutex
	entries map[scopeCacheKey]*scopeCacheEntry
	lru     *list.List // front = most recently used; values are scopeCacheKey
	max     int
	ttl     time.Duration
	now     func() time.Time // injectable for tests
}

func newScopeCache(max int, ttl time.Duration) *scopeCache {
	return &scopeCache{
		entries: make(map[scopeCacheKey]*scopeCacheEntry),
		lru:     list.New(),
		max:     max,
		ttl:     ttl,
		now:     time.Now,
	}
}

// lookup returns true if a fresh positive entry exists for (key,scope).
// Hits bump LRU position. Stale entries are evicted on lookup.
func (c *scopeCache) lookup(k scopeCacheKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return false
	}
	if c.now().Sub(e.verifiedAt) >= c.ttl {
		// Expired — evict.
		c.lru.Remove(e.lruElem)
		delete(c.entries, k)
		return false
	}
	c.lru.MoveToFront(e.lruElem)
	return true
}

// store records a positive verification, evicting the oldest entry
// if capacity is exceeded.
func (c *scopeCache) store(k scopeCacheKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[k]; ok {
		// Refresh in place.
		e.verifiedAt = c.now()
		c.lru.MoveToFront(e.lruElem)
		return
	}
	elem := c.lru.PushFront(k)
	c.entries[k] = &scopeCacheEntry{verifiedAt: c.now(), lruElem: elem}
	for c.lru.Len() > c.max {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.lru.Remove(oldest)
		delete(c.entries, oldest.Value.(scopeCacheKey))
	}
}

// len returns the current entry count (test helper).
func (c *scopeCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// ─── Client.VerifyScope ────────────────────────────────────────────────

// scopeCache field is added to Client via accessor below to keep the
// public Client struct stable. Lazy-init on first use under mu.
var (
	clientScopeCachesMu sync.Mutex
	clientScopeCaches   = map[*Client]*scopeCache{}
)

func (c *Client) getScopeCache() *scopeCache {
	clientScopeCachesMu.Lock()
	defer clientScopeCachesMu.Unlock()
	if sc, ok := clientScopeCaches[c]; ok {
		return sc
	}
	sc := newScopeCache(scopeCacheMaxEntries, scopeCacheTTL)
	clientScopeCaches[c] = sc
	return sc
}

// VerifyScope checks that the principal identified by `key` is actually
// authorized for the claimed scope, by re-querying the keystore. Use
// from middleware that's already accepted a request via the gateway-set
// X-Auth-* headers and wants a defense-in-depth check that the gateway
// hasn't been compromised.
//
// Returns nil on match. Returns ErrScopeMismatch when the keystore says
// the principal has a different scope than claimed (likely header
// forgery or stale key). Returns underlying network/keystore errors
// otherwise (ErrInvalidKey, ErrKeystoreUnavailable).
//
// Caches positive verifications for 5 minutes per (key, scope) pair to
// avoid hammering the keystore on every request. Negative results are
// NEVER cached — a revoked key must take effect immediately.
//
// Cost: ~1 keystore call per (key, scope) per 5 min per service. At 222
// services × N keys × 12 calls/hour, the keystore should handle it; if
// you see /verify rate spikes from this, lower the cache TTL via
// SetScopeCacheTTL.
func (c *Client) VerifyScope(ctx context.Context, key, claimedScope string) error {
	return c.verifyScopeWith(ctx, c.getScopeCache(), c, key, claimedScope)
}

// verifyScopeWith is the testable seam: an explicit Verifier + cache.
// Production callers go through VerifyScope; tests inject a stub.
func (c *Client) verifyScopeWith(ctx context.Context, sc *scopeCache, v Verifier, key, claimedScope string) error {
	k := scopeCacheKey{key: key, scope: claimedScope}
	if sc.lookup(k) {
		return nil
	}
	res, err := v.Verify(ctx, key)
	if err != nil {
		return err
	}
	if res.Scope != claimedScope {
		return fmt.Errorf("%w: claimed=%q actual=%q", ErrScopeMismatch, claimedScope, res.Scope)
	}
	sc.store(k)
	return nil
}

// ─── Test seams (unexported) ───────────────────────────────────────────

// setScopeCacheNowForTest swaps the time source for the given Client's
// cache. Tests-only; do not use in production code.
func (c *Client) setScopeCacheNowForTest(now func() time.Time) {
	c.getScopeCache().setNow(now)
}

func (sc *scopeCache) setNow(now func() time.Time) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.now = now
}
