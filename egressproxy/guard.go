package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
)

var (
	// ErrDenied marks a target the guard refuses (answered 403). Custom guards
	// may return it or any other error; only ErrGuardUnavailable changes the
	// answer.
	ErrDenied = errors.New("egressproxy: target denied by the SSRF guard")
	// ErrGuardUnavailable marks a guard that could not reach a verdict, for
	// example because the target name did not resolve. The request is refused
	// (fail closed) with 502 instead of 403, and the verdict is cached only
	// briefly.
	ErrGuardUnavailable = errors.New("egressproxy: SSRF guard could not decide")
)

// unavailableTTL is how long an ErrGuardUnavailable verdict is remembered. It
// only has to absorb a burst of subresources for one dead host.
const unavailableTTL = 2 * time.Second

// DefaultGuard is the guard used when Config.Guard is nil. It reuses
// safehttp.GuardHost, so it rejects literal loopback, private, link-local
// (cloud metadata 169.254.169.254), CGNAT and ULA addresses, hostnames that
// resolve to any of them, and the compliance denylist, and it honours the same
// SAFEHTTP_ALLOW_PRIVATE_IPS / SAFEHTTP_ALLOW_PRIVATE_HOSTS allow-lists.
//
// The upstream proxy resolves the target name itself, so this check cannot
// close a DNS-rebinding gap; it stops the browser from being steered at
// internal address literals and at names that already point inside.
func DefaultGuard(ctx context.Context, host, port string) error {
	err := safehttp.GuardHost(ctx, host)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, safehttp.ErrBlocked),
		errors.Is(err, safehttp.ErrDomainDenied),
		errors.Is(err, safehttp.ErrMissingHost):
		return fmt.Errorf("%w: %v", ErrDenied, err)
	default:
		return fmt.Errorf("%w: %v", ErrGuardUnavailable, err)
	}
}

type guardEntry struct {
	err error
	exp time.Time
}

// guardCache remembers guard verdicts per host:port for a short time with a
// hard size bound (cleared wholesale when full, like safehttp's own cache), so
// a page with hundreds of subresources does not resolve the same name hundreds
// of times.
type guardCache struct {
	mu  sync.Mutex
	m   map[string]guardEntry
	ttl time.Duration
	cap int
	now func() time.Time
}

func newGuardCache(ttl time.Duration, capacity int, now func() time.Time) *guardCache {
	return &guardCache{m: make(map[string]guardEntry), ttl: ttl, cap: capacity, now: now}
}

func guardKey(host, port string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".") + ":" + port
}

func (c *guardCache) check(ctx context.Context, g Guard, host, port string) error {
	key := guardKey(host, port)
	now := c.now()
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if ok && now.Before(e.exp) {
		return e.err
	}
	err := g(ctx, host, port)
	if ctx.Err() != nil {
		// The request was abandoned mid-lookup: nothing definitive to remember.
		return err
	}
	ttl := c.ttl
	if errors.Is(err, ErrGuardUnavailable) {
		ttl = min(ttl, unavailableTTL)
	}
	c.mu.Lock()
	if len(c.m) >= c.cap {
		c.m = make(map[string]guardEntry)
	}
	c.m[key] = guardEntry{err: err, exp: c.now().Add(ttl)}
	c.mu.Unlock()
	return err
}

func (c *guardCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
