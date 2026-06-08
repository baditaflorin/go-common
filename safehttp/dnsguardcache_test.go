package safehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestControlOverridesStaleAllowedCache is the load-bearing safety test: it
// poisons the verdict cache with an "allowed" entry for a hostname that
// resolves to loopback, then drives a real request through NewClient and
// asserts it is STILL blocked — proving the Dialer.Control re-check on the
// actually-connected IP, not the (now cached) GuardHost verdict, is the
// enforcement boundary. If this fails, the cache would be an SSRF bypass.
func TestControlOverridesStaleAllowedCache(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	// httptest listens on 127.0.0.1:PORT; reach it via a hostname so the
	// cache (which skips IP literals) is actually consulted.
	hostURL := "http://localhost:" + u.Port() + "/"

	defaultGuardCache.put("localhost", nil) // poison: claim loopback host is allowed
	defer func() {
		defaultGuardCache.mu.Lock()
		delete(defaultGuardCache.m, "localhost")
		defaultGuardCache.mu.Unlock()
	}()
	if verdict, ok := defaultGuardCache.get("localhost"); !ok || verdict != nil {
		t.Fatalf("precondition: cache should report allowed, got (%v,%v)", verdict, ok)
	}

	c := NewClient(WithUserAgent("test/1.0"))
	if _, err := c.Get(hostURL); err == nil {
		t.Fatal("request to loopback succeeded despite poisoned allow-cache — Control did NOT re-check the connected IP (SSRF bypass)")
	} else if !errors.Is(err, ErrBlocked) && !strings.Contains(strings.ToLower(err.Error()), "blocked") {
		t.Errorf("expected ErrBlocked from Dialer.Control, got: %v", err)
	}
}

// TestGuardCacheKeyNormalization: case and trailing-dot variants share one
// cache entry, so a verdict cached under one form is served for the others.
func TestGuardCacheKeyNormalization(t *testing.T) {
	c := newGuardHostCache(time.Minute, 8192)
	c.put("Example.COM.", ErrBlocked)
	for _, variant := range []string{"example.com", "EXAMPLE.com", "example.com.", "Example.COM"} {
		if verdict, ok := c.get(variant); !ok || verdict != ErrBlocked {
			t.Errorf("get(%q) = (%v,%v), want (ErrBlocked,true)", variant, verdict, ok)
		}
	}
}

func TestGuardCacheHitMissExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newGuardHostCache(30*time.Second, 8192)
	c.now = func() time.Time { return now }

	if _, ok := c.get("a.example"); ok {
		t.Fatal("expected miss on empty cache")
	}
	c.put("a.example", nil)        // allowed
	c.put("b.example", ErrBlocked) // blocked

	if err, ok := c.get("a.example"); !ok || err != nil {
		t.Errorf("a.example: got (%v,%v), want (nil,true)", err, ok)
	}
	if err, ok := c.get("b.example"); !ok || err != ErrBlocked {
		t.Errorf("b.example: got (%v,%v), want (ErrBlocked,true)", err, ok)
	}

	// Advance past TTL → entries expire.
	now = now.Add(31 * time.Second)
	if _, ok := c.get("a.example"); ok {
		t.Error("a.example should have expired")
	}
}

func TestGuardCacheCapClears(t *testing.T) {
	c := newGuardHostCache(time.Minute, 4)
	for i := 0; i < 4; i++ {
		c.put(string(rune('a'+i))+".example", nil)
	}
	// 5th put hits the cap and clears, then stores the new one.
	c.put("overflow.example", nil)
	if _, ok := c.get("overflow.example"); !ok {
		t.Error("overflow entry should be present after cap-clear")
	}
	if got := len(c.m); got != 1 {
		t.Errorf("after cap-clear len = %d, want 1", got)
	}
}

func TestGuardHostUsesCache(t *testing.T) {
	// Seed the package cache with a blocked verdict for a host that does
	// NOT resolve — proving GuardHost returns the cached verdict without a
	// DNS lookup (a real lookup would fail with a dns error, not ErrBlocked).
	const host = "cached-blocked.invalid.test"
	defaultGuardCache.put(host, ErrBlocked)
	defer func() {
		defaultGuardCache.mu.Lock()
		delete(defaultGuardCache.m, host)
		defaultGuardCache.mu.Unlock()
	}()

	if err := GuardHost(context.Background(), host); err != ErrBlocked {
		t.Errorf("GuardHost(%q) = %v, want ErrBlocked from cache", host, err)
	}
}

func TestGuardHostIPLiteralBypassesCache(t *testing.T) {
	// IP literals must be checked directly, never cached/looked up.
	if err := GuardHost(context.Background(), "127.0.0.1"); err != ErrBlocked {
		t.Errorf("loopback IP: got %v, want ErrBlocked", err)
	}
	if err := GuardHost(context.Background(), "8.8.8.8"); err != nil {
		t.Errorf("public IP: got %v, want nil", err)
	}
}
