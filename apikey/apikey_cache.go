package apikey

import (
	"context"
	"errors"
	"sync"
	"time"
)

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
