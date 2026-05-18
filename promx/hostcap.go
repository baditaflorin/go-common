package promx

import "sync"

// hostCardCap bounds the number of distinct "host" label values emitted by
// a collector. Once the cap is reached, every new host folds to "_other".
// Already-seen hosts always resolve to themselves, so a long-running fleet
// service has stable label sets even after the cap fills.
//
// This is deliberately cheap: a single sync.RWMutex around a map. Per-call
// overhead is one read-locked lookup on the hot path. Hosts are bounded
// strings (hostnames), so memory usage is O(limit * avg_hostname_len) ≈
// a few KB at typical caps.
type hostCardCap struct {
	limit int
	mu    sync.RWMutex
	seen  map[string]struct{}
}

func newHostCardCap(limit int) *hostCardCap {
	if limit <= 0 {
		limit = 256
	}
	return &hostCardCap{limit: limit, seen: make(map[string]struct{}, limit)}
}

// label returns the label to use for h: either h itself (if it's already
// been admitted, or there's room to admit it) or the literal "_other".
// Empty hosts collapse to "_unknown" so we never emit an empty label.
func (c *hostCardCap) label(h string) string {
	if h == "" {
		return "_unknown"
	}
	c.mu.RLock()
	if _, ok := c.seen[h]; ok {
		c.mu.RUnlock()
		return h
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[h]; ok {
		return h
	}
	if len(c.seen) >= c.limit {
		return "_other"
	}
	c.seen[h] = struct{}{}
	return h
}
