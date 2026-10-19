package promx

import "sync"

// Defaults for the "host" label of the egress and fleetfetch collectors.
//
// Measured on the Metrics Hub Prometheus on 2026-10-19, under the old rule (the
// first 256 hosts a process saw, kept for its lifetime): 92 of the 130 processes
// exporting safehttp_egress_requests_total sat at the cap, 25k host values in
// all, yet 94% of those processes' requests already counted under "_other"
// (97% for fleet_fetch_total): their 256 named hosts were just the first
// domains crawled after boot, a different set after every restart. The hosts
// worth a name are few: among the named hosts, 103 of the 130 processes had
// none with 64 or more requests, and the most any process had was 44.
const (
	defaultHostLimit   = 32
	defaultHostMinHits = 64
	// hostCandidateWindow bounds how many hosts are tracked while they earn a slot.
	hostCandidateWindow = 1024
)

// hostCardCap bounds the number of distinct values a collector emits for one
// label. It admits at most limit values for the life of the process and never
// evicts one, so an admitted value always resolves to itself (its series stay
// stable) and the label takes at most limit+2 distinct values whatever it is fed:
// the admitted ones, "_other" and "_unknown" (for an empty value).
//
// minHits says how a value earns one of the limit slots:
//
//   - minHits <= 1: the first limit distinct values win. Right for a label whose
//     values are few and all wanted, such as the HTTP route.
//   - minHits > 1: a value is admitted on its minHits-th observation; the
//     observations before that are reported as "_other", so a sum over the label
//     stays exact. The slots go to the values that recur, not to the first ones
//     seen. Right for a long-tailed label such as the hosts a crawler fetches.
//
// Values still earning a slot are counted in a map of at most window entries.
// When a new value finds it full, the map is cleared and counting starts again,
// so a value has to reach minHits before window other new values appear: a
// domain crawled a few times does not, an upstream called for every domain does.
// Memory stays O(limit + window) hostnames.
//
// An admitted value, and any value once every slot is taken, costs one
// read-locked map lookup.
type hostCardCap struct {
	limit   int
	minHits int
	window  int

	mu         sync.RWMutex
	seen       map[string]struct{}
	candidates map[string]int // nil unless minHits > 1 and a slot is free
}

// newHostCardCap returns a first-come cap of limit values (256 when limit <= 0).
func newHostCardCap(limit int) *hostCardCap {
	return newHeavyHitterCap(limit, 1, 0)
}

// newHeavyHitterCap returns a cap of limit values (256 when limit <= 0) that
// admits a value on its minHits-th observation within a window of window
// candidates (hostCandidateWindow when window <= 0).
func newHeavyHitterCap(limit, minHits, window int) *hostCardCap {
	if limit <= 0 {
		limit = 256
	}
	if window <= 0 {
		window = hostCandidateWindow
	}
	c := &hostCardCap{limit: limit, minHits: minHits, window: window, seen: make(map[string]struct{}, limit)}
	if minHits > 1 {
		c.candidates = make(map[string]int)
	}
	return c
}

// label returns the label to use for h: h itself when it is admitted (or is
// admitted by this call), "_other" when it is not, "_unknown" when h is empty.
func (c *hostCardCap) label(h string) string {
	if h == "" {
		return "_unknown"
	}
	c.mu.RLock()
	_, ok := c.seen[h]
	full := len(c.seen) >= c.limit
	c.mu.RUnlock()
	if ok {
		return h
	}
	if full {
		return "_other"
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[h]; ok {
		return h
	}
	if len(c.seen) >= c.limit {
		return "_other"
	}
	if c.minHits > 1 {
		n, tracked := c.candidates[h]
		if !tracked && len(c.candidates) >= c.window {
			clear(c.candidates)
		}
		if n+1 < c.minHits {
			c.candidates[h] = n + 1
			return "_other"
		}
		delete(c.candidates, h)
	}
	c.seen[h] = struct{}{}
	if len(c.seen) >= c.limit {
		// Nothing more can be admitted, and every later call returns before
		// the candidate code above: let the window go.
		c.candidates = nil
	}
	return h
}
