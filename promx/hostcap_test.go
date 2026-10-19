package promx

import (
	"fmt"
	"sync"
	"testing"

	"github.com/baditaflorin/go-common/safehttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestHeavyHitterCapAdmitsOnMinHits: a host is admitted on its minHits-th
// observation, the ones before count as "_other", and once admitted it stays.
func TestHeavyHitterCapAdmitsOnMinHits(t *testing.T) {
	c := newHeavyHitterCap(2, 3, 0)
	for i, want := range []string{"_other", "_other", "a", "a"} {
		if got := c.label("a"); got != want {
			t.Errorf("call %d: label(a) = %q, want %q", i+1, got, want)
		}
	}
	if got := c.label(""); got != "_unknown" {
		t.Errorf("empty host should become _unknown, got %q", got)
	}
}

// TestHeavyHitterCapWindowRestartsCount: when the candidate window is full, a
// new host clears it, so a host has to reach minHits before window other new
// hosts appear. Without the clear, "a" would be admitted on its second call.
func TestHeavyHitterCapWindowRestartsCount(t *testing.T) {
	c := newHeavyHitterCap(4, 2, 2)
	for _, h := range []string{"a", "b", "c"} { // c finds the window full: a's count is dropped
		if got := c.label(h); got != "_other" {
			t.Fatalf("first sight of %q = %q, want _other", h, got)
		}
	}
	if got := c.label("a"); got != "_other" {
		t.Errorf("a after the window restarted = %q, want _other (its first count was dropped)", got)
	}
	if got := c.label("a"); got != "a" {
		t.Errorf("a on its second call in the new window = %q, want a", got)
	}
}

// TestHostCardCapBound: whatever the input, a cap of limit emits at most limit
// admitted values plus "_other" and "_unknown", and tracks at most window
// candidates. The inputs include the worst case for each rule: every host new
// (first-come fills at once), and every host repeated exactly minHits times in a
// row (every one of them qualifies for the heavy-hitter rule).
func TestHostCardCapBound(t *testing.T) {
	const limit, minHits, window, calls = 8, 3, 16, 50000
	caps := []struct {
		name string
		new  func() *hostCardCap
	}{
		{"first-come", func() *hostCardCap { return newHostCardCap(limit) }},
		{"heavy-hitter", func() *hostCardCap { return newHeavyHitterCap(limit, minHits, window) }},
	}
	// heavyFills: whether the input has hosts that recur often enough to fill
	// the heavy-hitter cap. A host seen once, or twice, or once per 1000 calls
	// (the window of 16 restarts its count) never earns a slot.
	inputs := []struct {
		name       string
		host       func(i int) string
		heavyFills bool
	}{
		{"every host new", func(i int) string { return fmt.Sprintf("h%d", i) }, false},
		{"each host minHits times in a row", func(i int) string { return fmt.Sprintf("h%d", i/minHits) }, true},
		{"round robin over 1000 hosts", func(i int) string { return fmt.Sprintf("h%d", i%1000) }, false},
		{"empty hosts mixed in", func(i int) string {
			if i%7 == 0 {
				return ""
			}
			return fmt.Sprintf("h%d", i/2)
		}, false},
	}
	for _, cp := range caps {
		for _, in := range inputs {
			t.Run(cp.name+"/"+in.name, func(t *testing.T) {
				c := cp.new()
				labels := map[string]bool{}
				for i := 0; i < calls; i++ {
					labels[c.label(in.host(i))] = true
					if len(c.candidates) > window {
						t.Fatalf("call %d: %d candidates tracked, want at most %d", i, len(c.candidates), window)
					}
				}
				named := 0
				for l := range labels {
					if l != "_other" && l != "_unknown" {
						named++
					}
				}
				if named > limit || len(labels) > limit+2 {
					t.Errorf("%d named values (%d in all), want at most %d (%d in all)", named, len(labels), limit, limit+2)
				}
				wantNamed := limit
				if cp.name == "heavy-hitter" && !in.heavyFills {
					wantNamed = 0
				}
				if named != wantNamed {
					t.Errorf("%d named values, want %d for this input", named, wantNamed)
				}
			})
		}
	}
}

// TestHostCardCapBoundConcurrent: the bound holds under concurrent callers
// (run with -race in CI): the fast path reads "full" under the read lock and
// the slow path re-checks it under the write lock.
func TestHostCardCapBoundConcurrent(t *testing.T) {
	const limit = 16
	c := newHeavyHitterCap(limit, 2, 64)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				c.label(fmt.Sprintf("h%d", (i*(g+1))%3000))
			}
		}(g)
	}
	wg.Wait()
	if n := len(c.seen); n > limit {
		t.Errorf("%d hosts admitted, want at most %d", n, limit)
	}
}

// TestEgressHostAdmission: with the default options a host's first requests
// count under host="_other", and it gets its own value on request 64.
func TestEgressHostAdmission(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewEgressCollectors(reg)
	ev := safehttp.EgressEvent{Host: "api.example.com", Scheme: "https", Outcome: safehttp.OutcomeSuccess, Bytes: 10, Channel: "direct"}
	for i := 0; i < defaultHostMinHits-1; i++ {
		coll.ObserveEgress(ev)
	}
	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, "_other", "https", "false", "success")); got != defaultHostMinHits-1 {
		t.Errorf(`requests_total{host="_other"} = %v, want %d`, got, defaultHostMinHits-1)
	}
	coll.ObserveEgress(ev)
	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, "api.example.com", "https", "false", "success")); got != 1 {
		t.Errorf(`requests_total{host="api.example.com"} = %v, want 1 (admitted on request %d)`, got, defaultHostMinHits)
	}
}
