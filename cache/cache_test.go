package cache_test

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/cache"
	"github.com/baditaflorin/go-common/clock"
)

var epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func newMock() *clock.Mock {
	return clock.NewMock(epoch)
}

func TestSetAndGet(t *testing.T) {
	mc := newMock()
	c := cache.New[string, int](10, time.Minute, cache.WithClock[string, int](mc))
	c.Set("a", 42)
	v, ok := c.Get("a")
	if !ok || v != 42 {
		t.Fatalf("Get: got (%v, %v), want (42, true)", v, ok)
	}
}

func TestTTLExpiry(t *testing.T) {
	mc := newMock()
	c := cache.New[string, int](10, time.Minute, cache.WithClock[string, int](mc))
	c.Set("a", 1)
	mc.Advance(2 * time.Minute) // past TTL
	_, ok := c.Get("a")
	if ok {
		t.Fatal("entry should be expired")
	}
}

func TestStaleWindow(t *testing.T) {
	mc := newMock()
	c := cache.New[string, int](10, time.Minute,
		cache.WithClock[string, int](mc),
		cache.WithStaleWindow[string, int](10*time.Minute),
	)
	c.Set("a", 99)
	mc.Advance(5 * time.Minute) // past TTL, inside stale window

	entry, ok := c.GetEntry("a")
	if !ok {
		t.Fatal("expected ok=true within stale window")
	}
	if !entry.Stale {
		t.Fatal("expected Stale=true")
	}
	if entry.Value != 99 {
		t.Fatalf("got value %d, want 99", entry.Value)
	}

	// Direct Get should be a miss since stale is excluded.
	_, freshOK := c.Get("a")
	if freshOK {
		t.Fatal("Get should return false for stale entry")
	}
}

func TestStaleWindowExpired(t *testing.T) {
	mc := newMock()
	c := cache.New[string, int](10, time.Minute,
		cache.WithClock[string, int](mc),
		cache.WithStaleWindow[string, int](5*time.Minute),
	)
	c.Set("a", 1)
	mc.Advance(10 * time.Minute) // past TTL+stale window

	_, ok := c.GetEntry("a")
	if ok {
		t.Fatal("should be fully evicted past stale window")
	}
}

func TestLRUEviction(t *testing.T) {
	mc := newMock()
	c := cache.New[int, string](3, time.Minute, cache.WithClock[int, string](mc))
	c.Set(1, "a")
	c.Set(2, "b")
	c.Set(3, "c")
	// Touch 1 so it is recently used; LRU is 2
	c.Get(1)
	c.Get(3)
	// Insert 4 → should evict 2 (LRU)
	c.Set(4, "d")
	_, ok := c.Get(2)
	if ok {
		t.Fatal("key 2 should have been evicted (LRU)")
	}
	if c.Len() != 3 {
		t.Fatalf("len = %d, want 3", c.Len())
	}
}

func TestDelete(t *testing.T) {
	mc := newMock()
	c := cache.New[string, int](10, time.Minute, cache.WithClock[string, int](mc))
	c.Set("x", 5)
	c.Delete("x")
	_, ok := c.Get("x")
	if ok {
		t.Fatal("deleted key should return miss")
	}
}

func TestPurge(t *testing.T) {
	mc := newMock()
	c := cache.New[string, int](10, time.Minute, cache.WithClock[string, int](mc))
	c.Set("a", 1)
	c.Set("b", 2)
	c.Purge()
	if c.Len() != 0 {
		t.Fatalf("after Purge len = %d, want 0", c.Len())
	}
}

func TestOverwrite(t *testing.T) {
	mc := newMock()
	c := cache.New[string, int](10, time.Minute, cache.WithClock[string, int](mc))
	c.Set("k", 1)
	c.Set("k", 2)
	v, ok := c.Get("k")
	if !ok || v != 2 {
		t.Fatalf("overwrite: got (%v, %v), want (2, true)", v, ok)
	}
	if c.Len() != 1 {
		t.Fatalf("overwrite should not grow len, got %d", c.Len())
	}
}
