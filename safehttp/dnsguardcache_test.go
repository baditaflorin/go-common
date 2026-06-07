package safehttp

import (
	"context"
	"testing"
	"time"
)

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
