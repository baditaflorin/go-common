package cache_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/cache"
)

func BenchmarkSetGet(b *testing.B) {
	c := cache.New[string, int](10_000, time.Hour)
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = "key-" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := keys[i%len(keys)]
		c.Set(k, i)
		if _, ok := c.Get(k); !ok {
			b.Fatal("miss right after Set")
		}
	}
}
