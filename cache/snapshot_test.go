package cache_test

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/cache"
)

func TestSnapshotPrunesExpiredAndIncludesStaleWithoutRefreshingLRU(t *testing.T) {
	mc := newMock()
	c := cache.New[string, int](2, time.Minute, cache.WithClock[string, int](mc), cache.WithStaleWindow[string, int](time.Minute))
	c.Set("first", 1)
	c.Set("second", 2)
	mc.Advance(time.Minute)
	snapshot := c.Snapshot()
	if len(snapshot) != 2 || !snapshot["first"].Stale {
		t.Fatalf("snapshot=%v", snapshot)
	}
	delete(snapshot, "second")
	c.Set("third", 3)
	if _, ok := c.GetEntry("first"); ok {
		t.Fatal("snapshot refreshed LRU position")
	}
	if _, ok := c.GetEntry("second"); !ok {
		t.Fatal("mutating snapshot changed cache")
	}
	mc.Advance(time.Minute)
	if got := c.Snapshot(); len(got) != 1 || got["third"].Value != 3 {
		t.Fatalf("snapshot after expiry=%v", got)
	}
	if c.Len() != 1 {
		t.Fatal("snapshot did not reclaim expired entry")
	}
}
