package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clearGuardCache resets the package cache so a test starts from a known
// state and does not leak verdicts into sibling tests.
func clearGuardCache(t *testing.T) {
	t.Helper()
	defaultGuardCache.mu.Lock()
	defaultGuardCache.m = make(map[string]guardVerdict)
	defaultGuardCache.mu.Unlock()
}

// TestRebindCachedAllowStillBlockedByControl is the core adversarial test:
// it proves a cached *allow* verdict for a hostname cannot let a connection
// reach a private IP, because the Dialer.Control re-check validates the
// actually-connected IP independently of the cache.
//
// Sequence modeled:
//
//	t0      attacker host resolves to a PUBLIC ip -> GuardHost allows, verdict cached
//	t0+10s  attacker rebinds host -> 127.0.0.1 (private)
//	t0+15s  a second request dials the host
//
// We model the rebind by (a) seeding the cache with an allow verdict for a
// hostname and (b) pointing that hostname at loopback at dial time
// (localhost -> 127.0.0.1). The dialer MUST still block via Control even
// though GuardHost short-circuits on the cached allow.
func TestRebindCachedAllowStillBlockedByControl(t *testing.T) {
	clearGuardCache(t)
	t.Cleanup(func() { clearGuardCache(t) })

	// Loopback test server: its real listener IP is 127.0.0.1, which is
	// what localhost resolves to. This is the post-rebind private address.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("parse test server addr: %v", err)
	}

	const host = "localhost" // resolves to 127.0.0.1 (private) at dial time

	// Poison the cache: pretend t0 resolved to a public IP and we cached an
	// allow. If the cache were the boundary, the dial would now succeed.
	defaultGuardCache.put(host, nil)
	if verdict, ok := defaultGuardCache.get(host); !ok || verdict != nil {
		t.Fatalf("precondition: cache should hold an allow verdict, got (%v,%v)", verdict, ok)
	}

	dial := makeDialer(false)
	conn, derr := dial(context.Background(), "tcp", host+":"+port)
	if conn != nil {
		conn.Close()
	}
	if derr == nil {
		t.Fatal("REBIND HOLE: cached allow let the dialer connect to a private IP; Control re-check did not fire")
	}
	if !errors.Is(derr, ErrBlocked) {
		t.Fatalf("expected ErrBlocked from Control re-check, got: %v", derr)
	}
}

// TestRebindThroughNewClientStillBlocked exercises the same property through
// the full NewClient transport (not just the raw dialer), with the cache
// pre-poisoned to allow. The end-to-end request must still be SSRF-blocked.
func TestRebindThroughNewClientStillBlocked(t *testing.T) {
	clearGuardCache(t)
	t.Cleanup(func() { clearGuardCache(t) })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("parse test server addr: %v", err)
	}

	defaultGuardCache.put("localhost", nil) // poison: cached allow

	c := NewClient(WithUserAgent("test/1.0"))
	resp, gerr := c.Get("http://localhost:" + port + "/")
	if resp != nil {
		resp.Body.Close()
	}
	if gerr == nil {
		t.Fatal("REBIND HOLE: cached allow let NewClient reach a loopback server")
	}
	if !strings.Contains(strings.ToLower(gerr.Error()), "blocked") {
		t.Errorf("expected blocked error, got: %v", gerr)
	}
}

// TestGuardHostTransientErrorNotCached proves a transient DNS failure is
// never stored, so a later (recovered) resolution is re-evaluated rather
// than inheriting a poisoned deny/allow.
func TestGuardHostTransientErrorNotCached(t *testing.T) {
	clearGuardCache(t)
	t.Cleanup(func() { clearGuardCache(t) })

	// A syntactically-valid host that does not resolve -> transient dns error.
	const host = "definitely-does-not-exist.invalid.test"
	if err := GuardHost(context.Background(), host); err == nil {
		t.Fatalf("expected dns lookup failure for %q", host)
	} else if errors.Is(err, ErrBlocked) {
		t.Fatalf("a non-resolving host must be a transient dns error, not a cached ErrBlocked: %v", err)
	}
	if _, ok := defaultGuardCache.get(host); ok {
		t.Error("transient DNS failure must NOT be cached")
	}
}

// TestGuardCacheGetExpiryBoundary checks the exact TTL boundary: an entry is
// valid up to and including exp, and a miss strictly after exp. Guards
// against an off-by-one that could retain a stale allow.
func TestGuardCacheGetExpiryBoundary(t *testing.T) {
	base := time.Unix(2000, 0)
	c := newGuardHostCache(30*time.Second, 8192)
	c.now = func() time.Time { return base }
	c.put("h.example", nil)

	// exactly at exp -> still valid (After(exp) is false at exp)
	c.now = func() time.Time { return base.Add(30 * time.Second) }
	if _, ok := c.get("h.example"); !ok {
		t.Error("entry should still be valid exactly at exp")
	}
	// one ns past exp -> miss
	c.now = func() time.Time { return base.Add(30*time.Second + 1) }
	if _, ok := c.get("h.example"); ok {
		t.Error("entry should be expired one ns past exp")
	}
}

// TestGuardCacheConcurrent runs concurrent get/put under -race to prove the
// RWMutex discipline holds (the cache is a process-global shared by every
// safehttp client in a service).
func TestGuardCacheConcurrent(t *testing.T) {
	c := newGuardHostCache(time.Minute, 1024)
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(id int) {
			for i := 0; i < 2000; i++ {
				h := string(rune('a'+(i%16))) + ".example"
				if i%2 == 0 {
					c.put(h, nil)
				} else {
					_, _ = c.get(h)
				}
			}
			done <- struct{}{}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}
