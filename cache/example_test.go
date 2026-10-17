package cache_test

import (
	"fmt"
	"time"

	"github.com/baditaflorin/go-common/cache"
	"github.com/baditaflorin/go-common/clock"
)

// The cache is an LRU with a TTL. Give it a mock clock and expiry becomes
// deterministic: no sleeps in the test.
func ExampleNew() {
	clk := clock.NewMock(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	c := cache.New[string, string](2, time.Minute, cache.WithClock[string, string](clk))

	c.Set("a", "1")
	c.Set("b", "2")
	c.Set("c", "3") // over capacity: the least recently used key, "a", is evicted

	_, okA := c.Get("a")
	vC, okC := c.Get("c")
	fmt.Println(okA, vC, okC, c.Len())

	clk.Advance(2 * time.Minute)
	_, okC = c.Get("c")
	fmt.Println(okC)
	// Output:
	// false 3 true 2
	// false
}
