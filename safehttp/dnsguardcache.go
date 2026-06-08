package safehttp

import (
	"strings"
	"sync"
	"time"
)

// guardCacheTTL is how long a GuardHost verdict (allowed / blocked) is
// trusted before the host is re-resolved. Short by design: the real
// rebind boundary is the Dialer.Control re-check on the actually-connected
// IP (see makeDialer), so this cache only saves a redundant DNS round-trip
// on the validation path (CheckURL) and between CheckURL and the dialer —
// it is NOT the security boundary. Keep it small.
const guardCacheTTL = 30 * time.Second

// guardCacheCap bounds the number of distinct hosts cached so a recon /
// crawl service hitting millions of distinct hosts cannot grow this map
// without limit. When the cap is hit the cache is cleared wholesale (a
// coarse but O(1)-amortised bound); correctness is unaffected because a
// miss simply re-resolves.
const guardCacheCap = 8192

type guardVerdict struct {
	err error // nil = allowed, ErrBlocked = blocked. Transient DNS errors are never cached.
	exp time.Time
}

// guardHostCache caches definitive GuardHost verdicts for hostnames with a
// short TTL. IP-literal hosts are never cached (GuardHost handles them
// without a DNS lookup). Safe against DNS rebinding: the dialer's
// Dialer.Control validates the real connected IP independently of this
// cache, so a stale "allowed" verdict cannot let a connection reach a
// blocked IP.
type guardHostCache struct {
	mu  sync.RWMutex
	m   map[string]guardVerdict
	ttl time.Duration
	cap int
	now func() time.Time // injectable for tests
}

var defaultGuardCache = newGuardHostCache(guardCacheTTL, guardCacheCap)

func newGuardHostCache(ttl time.Duration, cap int) *guardHostCache {
	return &guardHostCache{
		m:   make(map[string]guardVerdict),
		ttl: ttl,
		cap: cap,
		now: time.Now,
	}
}

// normHost canonicalises a hostname for cache keying: lowercased and with a
// single trailing dot stripped, so "Example.COM", "example.com", and
// "example.com." share one entry. This only affects hit rate — a missed key
// just re-resolves — but it removes the "could two forms collide?" reasoning
// burden by making the key deterministic. IP literals are never cached, so
// no IPv6-bracket handling is needed here.
func normHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// get returns the cached verdict for host and whether it was a live hit.
func (c *guardHostCache) get(host string) (error, bool) {
	host = normHost(host)
	c.mu.RLock()
	v, ok := c.m[host]
	c.mu.RUnlock()
	if !ok || c.now().After(v.exp) {
		return nil, false
	}
	return v.err, true
}

// put stores a definitive verdict (allowed/blocked) for host. Callers must
// not cache transient DNS lookup failures.
func (c *guardHostCache) put(host string, verdict error) {
	host = normHost(host)
	c.mu.Lock()
	if len(c.m) >= c.cap {
		c.m = make(map[string]guardVerdict)
	}
	c.m[host] = guardVerdict{err: verdict, exp: c.now().Add(c.ttl)}
	c.mu.Unlock()
}
