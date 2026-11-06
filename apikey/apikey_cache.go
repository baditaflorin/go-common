package apikey

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/cache"
	"github.com/baditaflorin/go-common/clock"
)

// DefaultCacheMaxEntries bounds the positive cache when MaxEntries is unset.
const DefaultCacheMaxEntries = 4096

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
//
// Configuration fields must be set before the first Verify call.
type Cache struct {
	Inner    Verifier
	FreshTTL time.Duration // how long a cached hit counts as "fresh" (no upstream call)
	StaleTTL time.Duration // how long a cached hit can fall back to during upstream outage

	// Observer (optional) receives one CacheEvent per Verify call.
	// promx.NewAuthCollectors() returns an implementation that records
	// fleet-canonical Prometheus metrics (cache hit rate, stale-serve
	// rate during outages, upstream call latency).
	Observer CacheObserver

	// MaxEntries bounds cached positive results. Zero uses DefaultCacheMaxEntries.
	MaxEntries int

	mu      sync.Mutex
	entries *cache.Cache[string, cacheEntry]
	clk     clock.Clock
}

// Verify implements the Verifier interface with cache logic:
//   - cache hit < FreshTTL: return cached, no upstream call.
//   - otherwise: call upstream.
//   - upstream OK: cache + return.
//   - upstream ErrInvalidKey: clear cache + propagate (definitive 401).
//   - upstream ErrKeystoreUnavailable: if cache hit < StaleTTL, return
//     cached + log; otherwise propagate ErrKeystoreUnavailable.
func (c *Cache) Verify(ctx context.Context, key string) (*VerifyResult, error) {
	now := c.now()
	c.mu.Lock()
	var entry cacheEntry
	var hadEntry bool
	if c.entries != nil {
		entry, hadEntry = c.entries.Get(key)
	}
	c.mu.Unlock()
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
		c.storeLocked(key, cacheEntry{result: *res, verified: now})
		c.mu.Unlock()
		c.observe(CacheEvent{Result: CacheResultInnerOK, Duration: dur})
		return res, nil
	}
	if errors.Is(err, ErrInvalidKey) {
		// Definitive rejection — drop any cached entry for this key.
		c.mu.Lock()
		if c.entries != nil {
			c.entries.Delete(key)
		}
		c.mu.Unlock()
		c.observe(CacheEvent{Result: CacheResultInnerInvalid, Duration: dur})
		return nil, err
	}
	// Only an explicit keystore outage permits stale authentication. A caller
	// cancellation or unrelated verifier failure must remain an error, even
	// when joined with the outage sentinel.
	if errors.Is(err, ErrKeystoreUnavailable) && ctx.Err() == nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		hadEntry && now.Sub(entry.verified) < c.StaleTTL {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]time.Time{}
	if c.entries != nil {
		for key, entry := range c.entries.Snapshot() {
			out[key] = entry.Value.verified
		}
	}
	return out
}

func (c *Cache) now() time.Time {
	if c.clk != nil {
		return c.clk.Now()
	}
	return time.Now()
}

// storeLocked reuses the shared LRU/TTL primitive and allocates storage only
// after the first successful verification. Retain entries long enough for both
// configured windows, including configurations with FreshTTL greater than StaleTTL.
func (c *Cache) storeLocked(key string, entry cacheEntry) {
	if c.entries == nil {
		ttl := max(c.FreshTTL, c.StaleTTL)
		if ttl <= 0 {
			return
		}
		capacity := c.MaxEntries
		if capacity <= 0 {
			capacity = DefaultCacheMaxEntries
		}
		clk := c.clk
		if clk == nil {
			clk = clock.Real()
		}
		c.entries = cache.New[string, cacheEntry](capacity, ttl, cache.WithClock[string, cacheEntry](clk))
	}
	c.entries.Set(key, entry)
}
