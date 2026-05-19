// Package cache provides a generic, concurrency-safe LRU+TTL cache.
//
// The implementation is extracted and generalised from the positive-only
// LRU inside apikey/cache.go. At least three other packages
// (policyeval regexp cache, graph event dedup, fleetfetch) reimpose
// the same pattern; this replaces them all.
//
// Key properties:
//   - Generics: New[K, V] works with any comparable key and any value.
//   - LRU eviction: when maxSize is exceeded, the least-recently-used
//     entry is evicted.
//   - Per-entry TTL: entries older than ttl are treated as misses and
//     evicted lazily on next access.
//   - Optional stale-while-revalidate window (WithStaleWindow): entries
//     past their TTL but within stale window are returned with a
//     Stale=true flag so callers can serve them while refreshing.
//   - Clock-injectable: pass a clock.Clock for deterministic tests.
//
// Usage:
//
//	c := cache.New[string, *MyVal](1000, 5*time.Minute)
//	c.Set("key", &MyVal{})
//	val, ok := c.Get("key")
//
//	// with stale window:
//	c2 := cache.New[string, *MyVal](1000, 5*time.Minute,
//	    cache.WithStaleWindow[string, *MyVal](15*time.Minute))
//	entry, ok := c2.GetEntry("key")
//	if ok && entry.Stale { /* serve stale, trigger background refresh */ }
package cache

import (
	"container/list"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/clock"
)

// Entry is the value returned by GetEntry, carrying the value and
// freshness metadata.
type Entry[V any] struct {
	Value   V
	Stale   bool      // true when TTL expired but within stale window
	Expires time.Time // absolute expiry of this entry's fresh period
}

// Cache is a generic LRU+TTL cache.
type Cache[K comparable, V any] struct {
	mu          sync.Mutex
	maxSize     int
	ttl         time.Duration
	staleWindow time.Duration
	clk         clock.Clock

	items map[K]*list.Element
	lru   *list.List
}

type item[K comparable, V any] struct {
	key     K
	value   V
	expires time.Time
}

// Option configures a Cache at construction time.
type Option[K comparable, V any] func(*Cache[K, V])

// WithClock injects a clock.Clock for deterministic testing.
func WithClock[K comparable, V any](c clock.Clock) Option[K, V] {
	return func(cache *Cache[K, V]) { cache.clk = c }
}

// WithStaleWindow sets the duration beyond TTL during which a stale
// entry is returned (with Entry.Stale=true) instead of a miss.
// A stale window of zero (the default) means entries are hard-evicted
// at TTL expiry.
func WithStaleWindow[K comparable, V any](d time.Duration) Option[K, V] {
	return func(cache *Cache[K, V]) { cache.staleWindow = d }
}

// New creates a new Cache with the given max size and TTL.
// maxSize must be > 0; ttl must be > 0.
func New[K comparable, V any](maxSize int, ttl time.Duration, opts ...Option[K, V]) *Cache[K, V] {
	c := &Cache[K, V]{
		maxSize: maxSize,
		ttl:     ttl,
		items:   make(map[K]*list.Element, maxSize),
		lru:     list.New(),
		clk:     clock.Real(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Set stores value under key with a TTL starting from now.
// If the key already exists its value and TTL are refreshed and it is
// moved to the front of the LRU. If the cache is full, the LRU entry
// is evicted before inserting.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clk.Now()
	expires := now.Add(c.ttl)

	if el, ok := c.items[key]; ok {
		c.lru.MoveToFront(el)
		el.Value.(*item[K, V]).value = value
		el.Value.(*item[K, V]).expires = expires
		return
	}
	if c.lru.Len() >= c.maxSize {
		c.evictOldest()
	}
	el := c.lru.PushFront(&item[K, V]{key: key, value: value, expires: expires})
	c.items[key] = el
}

// Get retrieves the value for key. Returns the value and true if the
// entry exists and is fresh. Returns the zero value and false otherwise.
// Use GetEntry when you need stale-window semantics.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	entry, ok := c.GetEntry(key)
	if !ok || entry.Stale {
		var zero V
		return zero, false
	}
	return entry.Value, true
}

// GetEntry retrieves an entry for key including stale metadata.
// ok=true means the key was found (possibly stale if Stale=true).
// ok=false means the key is absent or expired beyond the stale window.
func (c *Cache[K, V]) GetEntry(key K) (Entry[V], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Entry[V]{}, false
	}
	it := el.Value.(*item[K, V])
	now := c.clk.Now()

	if now.Before(it.expires) {
		// Fresh hit.
		c.lru.MoveToFront(el)
		return Entry[V]{Value: it.value, Expires: it.expires}, true
	}

	// Expired. Check stale window.
	if c.staleWindow > 0 && now.Before(it.expires.Add(c.staleWindow)) {
		c.lru.MoveToFront(el)
		return Entry[V]{Value: it.value, Stale: true, Expires: it.expires}, true
	}

	// Beyond stale window — hard evict.
	c.evict(el)
	return Entry[V]{}, false
}

// Delete removes key from the cache. No-op if absent.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.evict(el)
	}
}

// Len returns the number of entries currently in the cache (including
// potentially expired entries not yet evicted).
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// Purge removes all entries.
func (c *Cache[K, V]) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[K]*list.Element, c.maxSize)
	c.lru.Init()
}

// evict removes el from both the map and LRU list. Must be called with c.mu held.
func (c *Cache[K, V]) evict(el *list.Element) {
	it := c.lru.Remove(el).(*item[K, V])
	delete(c.items, it.key)
}

// evictOldest removes the tail element (LRU). Must be called with c.mu held.
func (c *Cache[K, V]) evictOldest() {
	if tail := c.lru.Back(); tail != nil {
		c.evict(tail)
	}
}
