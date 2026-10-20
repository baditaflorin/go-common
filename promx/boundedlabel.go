package promx

import (
	"sync"
	"unicode/utf8"
)

// The values a BoundedLabel emits besides the ones it names.
const (
	// OtherLabel is what a BoundedLabel emits for every value it does not name: a
	// value past the limit, one that has not recurred often enough to earn a slot,
	// one that is not valid UTF-8 and one longer than MaxLabelValueLen. All of them
	// count under this one value, so a sum over the label stays exact.
	OtherLabel = "_other"
	// UnknownLabel is what a BoundedLabel emits for the empty string.
	UnknownLabel = "_unknown"
	// MaxLabelValueLen is the longest value, in bytes, that a BoundedLabel names.
	// A longer one counts under OtherLabel and is not stored. A hostname is at most
	// 253 bytes and a templated route is shorter, so this only ever turns away
	// input nobody meant as a label: a request path can be as long as the server's
	// header limit (1 MiB by default), and an admitted value is held for the life
	// of the process and written into every scrape.
	MaxLabelValueLen = 256
)

// Defaults for the "host" label of the egress and fleetfetch collectors, and the
// fallbacks of NewBoundedLabel.
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
	defaultLabelLimit  = 32
	defaultHostLimit   = defaultLabelLimit
	defaultHostMinHits = 64
	// defaultCandidateWindow bounds how many values are tracked while they earn a slot.
	defaultCandidateWindow = 1024
)

// BoundedLabel bounds how many distinct values one Prometheus label takes, however
// many distinct values the process is fed. It is the one supported way to put a
// value the process does not choose (a request path, a method, a host, a name taken
// from a URL or a header) in a label: every series is a copy of everything that
// carries that label, so one label that grows with traffic takes the scrape
// target, and the Prometheus that stores it, down with it. That happened twice in
// three days in October 2026 (the HTTP "method" label, then the "host" label on the
// egress and fleetfetch histograms), and BoundedLabel is what the second fix was
// made of, factored out so the next label does not need a third incident.
//
// It admits at most limit values for the life of the process and never evicts one,
// so an admitted value always resolves to itself (its series stay stable) and the
// label takes at most MaxValues distinct values whatever it is fed: the admitted
// ones, OtherLabel, and UnknownLabel for the empty string.
//
// How a value earns one of the limit slots depends on its minimum hits (WithMinHits):
//
//   - 1, the default: the first limit distinct values win. Right for a label whose
//     values are few and all wanted, such as the HTTP route.
//   - n > 1: a value is admitted on its n-th observation, and the observations before
//     that are reported as OtherLabel, so a sum over the label stays exact. The slots
//     go to the values that recur, not to the first ones seen. Right for a
//     long-tailed label such as the hosts a crawler fetches.
//
// Values still earning a slot are counted in a map of at most window entries
// (WithCandidateWindow). When a new value finds it full, the map is cleared and
// counting starts again, so a value has to reach its n hits before window other new
// values appear: a domain crawled a few times does not, an upstream called for every
// domain does. Memory stays O(limit + window) values of at most MaxLabelValueLen
// bytes. An admitted value, and any value once every slot is taken, costs one
// read-locked map lookup.
//
// A BoundedLabel is safe for concurrent use. Use one per label per collector and
// call Value on the way into WithLabelValues:
//
//	host := promx.NewBoundedLabel(32, promx.WithMinHits(64))
//	...
//	counter.WithLabelValues(service, host.Value(ev.Host)).Inc()
//
// A label whose value is one of a short fixed list needs no BoundedLabel: map it
// with a switch, as the "method" label does. A label that names something a service
// declares once at start-up (a pool, a gate) is configuration, not input, and its
// series count is the number of names declared. Whatever the label, a new metric's
// series budget goes in docs/observability.md, where TestSeriesBudgets checks it,
// and promxtest.AssertBounded (testhelpers/promxtest) drives hostile values through
// the collector to prove the bound holds.
type BoundedLabel struct {
	limit   int
	minHits int
	window  int

	mu         sync.RWMutex
	seen       map[string]struct{}
	candidates map[string]int // nil unless minHits > 1 and a slot is free
}

// BoundedLabelOption configures NewBoundedLabel.
type BoundedLabelOption func(*boundedLabelOpts)

type boundedLabelOpts struct {
	minHits int
	window  int
}

// WithMinHits sets how many times a value has to be seen before it gets its own
// label value; until then it counts under OtherLabel. n <= 1, the default, names the
// first values seen. It is the knob WithHostMinHits sets on the egress collectors.
func WithMinHits(n int) BoundedLabelOption {
	return func(o *boundedLabelOpts) { o.minHits = n }
}

// WithCandidateWindow sets how many not-yet-admitted values are tracked while they
// earn a slot (default 1024; n <= 0 keeps the default). It only matters with
// WithMinHits above 1.
func WithCandidateWindow(n int) BoundedLabelOption {
	return func(o *boundedLabelOpts) { o.window = n }
}

// NewBoundedLabel returns a BoundedLabel that names at most limit values. A limit
// <= 0 means the default, 32: there is no way to switch the bound off.
func NewBoundedLabel(limit int, opts ...BoundedLabelOption) *BoundedLabel {
	o := boundedLabelOpts{minHits: 1, window: defaultCandidateWindow}
	for _, opt := range opts {
		opt(&o)
	}
	if limit <= 0 {
		limit = defaultLabelLimit
	}
	if o.window <= 0 {
		o.window = defaultCandidateWindow
	}
	b := &BoundedLabel{limit: limit, minHits: o.minHits, window: o.window, seen: make(map[string]struct{}, min(limit, 64))}
	if o.minHits > 1 {
		b.candidates = make(map[string]int)
	}
	return b
}

// MaxValues is the most distinct values Value can return: the limit, OtherLabel and
// UnknownLabel. It is the factor this label contributes to a metric's series budget.
func (b *BoundedLabel) MaxValues() int { return b.limit + 2 }

// Value returns the label value to use for v: v itself when it is admitted (or is
// admitted by this call), OtherLabel when it is not, UnknownLabel when v is empty.
func (b *BoundedLabel) Value(v string) string {
	if v == "" {
		return UnknownLabel
	}
	// Before the map lookup, which would hash all of it.
	if len(v) > MaxLabelValueLen {
		return OtherLabel
	}
	b.mu.RLock()
	_, ok := b.seen[v]
	full := len(b.seen) >= b.limit
	b.mu.RUnlock()
	if ok {
		return v
	}
	// client_golang panics in WithLabelValues on a value that is not valid UTF-8,
	// and a request path (GET /%ff) or a host (http://%ff.example/) can be. An
	// admitted value never is: this is the only way in.
	if full || !utf8.ValidString(v) {
		return OtherLabel
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[v]; ok {
		return v
	}
	if len(b.seen) >= b.limit {
		return OtherLabel
	}
	if b.minHits > 1 {
		n, tracked := b.candidates[v]
		if !tracked && len(b.candidates) >= b.window {
			clear(b.candidates)
		}
		if n+1 < b.minHits {
			b.candidates[v] = n + 1
			return OtherLabel
		}
		delete(b.candidates, v)
	}
	b.seen[v] = struct{}{}
	if len(b.seen) >= b.limit {
		// Nothing more can be admitted, and every later call returns before
		// the candidate code above: let the window go.
		b.candidates = nil
	}
	return v
}
