package promx

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/baditaflorin/go-common/safehttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestHeavyHitterCapAdmitsOnMinHits: a host is admitted on its minHits-th
// observation, the ones before count as "_other", and once admitted it stays.
func TestHeavyHitterCapAdmitsOnMinHits(t *testing.T) {
	c := NewBoundedLabel(2, WithMinHits(3))
	for i, want := range []string{"_other", "_other", "a", "a"} {
		if got := c.Value("a"); got != want {
			t.Errorf("call %d: label(a) = %q, want %q", i+1, got, want)
		}
	}
	if got := c.Value(""); got != "_unknown" {
		t.Errorf("empty host should become _unknown, got %q", got)
	}
}

// TestHeavyHitterCapWindowRestartsCount: when the candidate window is full, a
// new host clears it, so a host has to reach minHits before window other new
// hosts appear. Without the clear, "a" would be admitted on its second call.
func TestHeavyHitterCapWindowRestartsCount(t *testing.T) {
	c := NewBoundedLabel(4, WithMinHits(2), WithCandidateWindow(2))
	for _, h := range []string{"a", "b", "c"} { // c finds the window full: a's count is dropped
		if got := c.Value(h); got != "_other" {
			t.Fatalf("first sight of %q = %q, want _other", h, got)
		}
	}
	if got := c.Value("a"); got != "_other" {
		t.Errorf("a after the window restarted = %q, want _other (its first count was dropped)", got)
	}
	if got := c.Value("a"); got != "a" {
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
		new  func() *BoundedLabel
	}{
		{"first-come", func() *BoundedLabel { return NewBoundedLabel(limit) }},
		{"heavy-hitter", func() *BoundedLabel { return NewBoundedLabel(limit, WithMinHits(minHits), WithCandidateWindow(window)) }},
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
					labels[c.Value(in.host(i))] = true
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
	c := NewBoundedLabel(limit, WithMinHits(2), WithCandidateWindow(64))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				c.Value(fmt.Sprintf("h%d", (i*(g+1))%3000))
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

// TestBoundedLabelRejectsUnusableValues: a value that client_golang would panic on
// (not valid UTF-8: GET /%ff gives a path of "/\xff", http://%ff.example/ a host
// of "\xff.example") or that is longer than MaxLabelValueLen is never named and
// never tracked, even when the label has room, under either admission rule.
func TestBoundedLabelRejectsUnusableValues(t *testing.T) {
	bad := []string{
		"\xff", "/\xff", "\xff.example", "\xc0\xaf", "\xed\xa0\x80", "ok\xe2\x82",
		strings.Repeat("a", MaxLabelValueLen+1), strings.Repeat("é", MaxLabelValueLen), strings.Repeat("x", 1<<20),
	}
	for name, c := range map[string]*BoundedLabel{
		"first-come":   NewBoundedLabel(8),
		"heavy-hitter": NewBoundedLabel(8, WithMinHits(2)),
	} {
		t.Run(name, func(t *testing.T) {
			for round := 0; round < 3; round++ {
				for _, v := range bad {
					if got := c.Value(v); got != OtherLabel {
						t.Fatalf("Value(%.20q, %d bytes) = %q, want %q", v, len(v), got, OtherLabel)
					}
				}
			}
			if len(c.seen) != 0 || len(c.candidates) != 0 {
				t.Errorf("%d admitted and %d tracked values after only unusable input, want none: they would be kept for the life of the process", len(c.seen), len(c.candidates))
			}
			// Room is not used up by them, and the longest usable value is still named.
			edge := strings.Repeat("h", MaxLabelValueLen)
			for i := 0; i < 2; i++ {
				c.Value(edge)
			}
			if got := c.Value(edge); got != edge {
				t.Errorf("a value of exactly MaxLabelValueLen bytes = %.20q, want it named", got)
			}
		})
	}
}

// TestBoundedLabelLimit: a limit <= 0 means the default, and MaxValues counts the
// two fold values.
func TestBoundedLabelLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		if got := NewBoundedLabel(limit).MaxValues(); got != defaultLabelLimit+2 {
			t.Errorf("NewBoundedLabel(%d).MaxValues() = %d, want %d", limit, got, defaultLabelLimit+2)
		}
	}
	if got := NewBoundedLabel(7).MaxValues(); got != 9 {
		t.Errorf("NewBoundedLabel(7).MaxValues() = %d, want 9", got)
	}
	// A huge limit does not pre-allocate for it.
	if got := NewBoundedLabel(1 << 30).MaxValues(); got != 1<<30+2 {
		t.Errorf("a huge limit = %d values", got)
	}
}

// FuzzBoundedLabel: whatever the input and the admission rule, Value returns the
// input itself (only when it is usable as a label value) or one of the two fold
// values, the result is always valid UTF-8 within MaxLabelValueLen, and the label
// takes at most MaxValues distinct values.
func FuzzBoundedLabel(f *testing.F) {
	for _, s := range []string{"", "a", "/x", "\xff", "é", "a\x00b", strings.Repeat("a", MaxLabelValueLen+1), "_other", "_unknown"} {
		f.Add(s, uint8(3))
	}
	f.Fuzz(func(t *testing.T, v string, minHits uint8) {
		b := NewBoundedLabel(3, WithMinHits(int(minHits%5)+1), WithCandidateWindow(6))
		got := map[string]bool{}
		for i := 0; i < 40; i++ {
			in := v
			if i%2 == 1 {
				in = v + strconv.Itoa(i/4) // a few related values, each seen repeatedly
			}
			out := b.Value(in)
			if !utf8.ValidString(out) || len(out) > MaxLabelValueLen {
				t.Fatalf("Value(%q) = %q: not a usable label value", in, out)
			}
			if out != in && out != OtherLabel && out != UnknownLabel {
				t.Fatalf("Value(%q) = %q: neither the input nor a fold value", in, out)
			}
			if in == "" && out != UnknownLabel {
				t.Fatalf("Value(empty) = %q, want %q", out, UnknownLabel)
			}
			got[out] = true
		}
		if len(got) > b.MaxValues() {
			t.Fatalf("%d distinct values, want at most %d", len(got), b.MaxValues())
		}
		if len(b.seen) > 3 || len(b.candidates) > 6 {
			t.Fatalf("%d admitted and %d tracked values, want at most 3 and 6", len(b.seen), len(b.candidates))
		}
	})
}
