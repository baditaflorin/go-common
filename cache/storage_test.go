package cache

import (
	"testing"
	"time"
)

var emptyCacheSink *Cache[int, int]

func TestEmptyCacheDoesNotReserveItsEntryLimit(t *testing.T) {
	for _, purge := range []bool{false, true} {
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				c := New[int, int](1_000_000, time.Hour)
				if purge {
					c.Set(1, 1)
					c.Purge()
				}
				emptyCacheSink = c
			}
		})
		if got := result.AllocedBytesPerOp(); got > 4<<10 {
			t.Fatalf("empty cache (purge=%v) allocated %d bytes, want <=4 KiB independent of limit", purge, got)
		}
	}
}

func BenchmarkCacheStorage(b *testing.B) {
	b.Run("NewEmpty", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			emptyCacheSink = New[int, int](10_000, time.Hour)
		}
	})
	b.Run("Purge", func(b *testing.B) {
		c := New[int, int](10_000, time.Hour)
		b.ReportAllocs()
		for b.Loop() {
			c.Purge()
		}
	})
	b.Run("Hit", func(b *testing.B) {
		c := New[int, int](10_000, time.Hour)
		c.Set(1, 1)
		b.ReportAllocs()
		for b.Loop() {
			if _, ok := c.Get(1); !ok {
				b.Fatal("miss")
			}
		}
	})
	b.Run("Churn", func(b *testing.B) {
		c := New[int, int](1024, time.Hour)
		key := 0
		b.ReportAllocs()
		for b.Loop() {
			c.Set(key, key)
			key++
		}
	})
}
