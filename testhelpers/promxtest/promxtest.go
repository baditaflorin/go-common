// Package promxtest tests that a Prometheus collector cannot be made to create an
// unbounded number of series: it drives hostile label values through the collector
// and counts what the registry would hand Prometheus.
//
// Every series is a copy of everything that carries it (a histogram costs one per
// bucket), so one label that takes a value from a request, a URL or a header costs
// the scrape target, and the Prometheus that stores it, memory in proportion to
// what an attacker sends. In October 2026 that took a Prometheus down twice in
// three days, and nothing had tested for it. AssertBounded is that test:
//
//	func TestEgressCollectorsAreBounded(t *testing.T) {
//	    promxtest.AssertBounded(t,
//	        func(reg prometheus.Registerer) *promx.EgressCollectors { return promx.NewEgressCollectors(reg) },
//	        []promxtest.Probe[*promx.EgressCollectors]{{
//	            Name: "hosts",
//	            Drive: func(c *promx.EgressCollectors, in *promxtest.Inputs) {
//	                in.Each(in.Hosts(), func(h string) {
//	                    c.ObserveEgress(safehttp.EgressEvent{Host: h, Scheme: "https", Outcome: safehttp.OutcomeSuccess})
//	                })
//	            },
//	        }},
//	        500) // series the collector may create, however hostile the input
//	}
//
// A probe is a loop that feeds one collector values from Inputs, the hostile corpus:
// many distinct hosts, methods, paths, schemes and user agents, values over 256
// bytes and values over 1 MiB, unicode, and values that are not valid UTF-8 (which
// client_golang panics on). AssertBounded runs the probes against a fresh registry
// twice, with the input at two sizes, and fails when
//
//   - a probe panics: a hostile value must never take the process down;
//   - the larger run created more series than the smaller: some label grows with its
//     input, which is what unbounded means, whatever the budget says;
//   - the run created more series than the budget;
//   - a label value is longer than the limit: it is held for the life of the
//     process and written into every scrape;
//   - the probes created no series at all, which is a probe that reaches nothing.
//
// Write a probe for every label whose value the process does not choose. A label
// that is one of a short fixed list needs no hostile probe, only a probe that uses
// every value on the list, so that the budget counts them.
//
// The package imports only the Prometheus client, so any package can use it,
// including promx's own tests.
package promxtest

import (
	"fmt"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Probe is one hostile workload for a collector set of type C.
type Probe[C any] struct {
	// Name says what the probe attacks ("hosts", "methods"), for failure messages.
	Name string
	// Drive feeds the collector values drawn from in: it observes events, serves
	// requests, whatever the collector takes. It runs on the calling goroutine.
	Drive func(c C, in *Inputs)
}

// Option adjusts AssertBounded.
type Option func(*config)

type config struct{ maxValueLen int }

// DefaultMaxValueLen is the longest label value AssertBounded accepts, in bytes. It
// is promx.MaxLabelValueLen: a hostname is at most 253 bytes, and anything longer is
// an input, not a name.
const DefaultMaxValueLen = 256

// WithMaxValueLen sets the longest label value, in bytes, that AssertBounded
// accepts (default DefaultMaxValueLen), for a collector whose names are longer.
func WithMaxValueLen(n int) Option { return func(c *config) { c.maxValueLen = n } }

// growScale is how much larger the second run's input is than the first's.
const growScale = 4

// AssertBounded registers a collector set in a fresh registry with register, runs
// every probe against it with hostile input, and fails the test if the collector
// can be made to create more than maxSeries series or an unbounded number (see the
// package comment for the five failures). It does this twice, with the input at
// scale 1 and at scale 4, each in a fresh registry, and returns the Report of the
// larger run.
//
// register is called once per run, and must register everything the probes
// exercise on the registerer it is given, and no other registry: the test depends
// on starting from nothing.
func AssertBounded[C any](t testing.TB, register func(prometheus.Registerer) C, probes []Probe[C], maxSeries int, opts ...Option) Report {
	t.Helper()
	cfg := config{maxValueLen: DefaultMaxValueLen}
	for _, o := range opts {
		o(&cfg)
	}

	run := func(scale int) Report {
		reg := prometheus.NewRegistry()
		Run(t, reg, register, probes, NewInputs(scale))
		return Measure(t, reg)
	}
	small, large := run(1), run(growScale)

	if large.Total == 0 {
		t.Fatalf("the probes created no series: they never reached the collector, so they measure nothing")
	}
	for _, m := range large.Metrics {
		sm, ok := small.Metric(m.Name)
		if ok && m.Series <= sm.Series {
			continue
		}
		t.Errorf("%s is not bounded: %d series with the input at scale 1, %d at scale %d. Labels that took more distinct values: %s.\n"+
			"Put the value through promx.BoundedLabel (or map it onto a fixed list) before it becomes a label value",
			m.Name, sm.Series, m.Series, growScale, grew(sm, m))
	}
	if worst := max(small.Total, large.Total); worst > maxSeries {
		t.Errorf("%d series, budget %d. Largest families:\n%s", worst, maxSeries, largest(large, 5))
	}
	for _, m := range large.Metrics {
		if m.MaxValueLen > cfg.maxValueLen {
			t.Errorf("%s has a label value of %d bytes, limit %d: it is held for the life of the process and written into every scrape", m.Name, m.MaxValueLen, cfg.maxValueLen)
		}
	}
	t.Logf("%s", large)
	return large
}

// Run registers a collector set on reg with register and drives every probe
// against it with in. A probe that panics fails the test and the run goes on with
// the next one. AssertBounded calls it for each of its two runs; call it directly to
// put several collector sets in one registry, as a budget test over a whole
// package does.
func Run[C any](t testing.TB, reg prometheus.Registerer, register func(prometheus.Registerer) C, probes []Probe[C], in *Inputs) {
	t.Helper()
	c := register(reg)
	for _, p := range probes {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("probe %q panicked: %v\n%s", p.Name, r, debug.Stack())
				}
			}()
			p.Drive(c, in)
		}()
	}
}

// grew lists the labels of m that took more distinct values than in sm.
func grew(sm, m Metric) string {
	var out []string
	for _, l := range m.Labels() {
		if m.Distinct(l) > sm.Distinct(l) {
			out = append(out, fmt.Sprintf("%s (%d to %d)", l, sm.Distinct(l), m.Distinct(l)))
		}
	}
	if len(out) == 0 {
		return "none, only more combinations of the same values"
	}
	return strings.Join(out, ", ")
}

func largest(r Report, n int) string {
	var b strings.Builder
	for _, m := range r.Largest(n) {
		fmt.Fprintf(&b, "  %-58s %8d series\n", m.Name, m.Series)
	}
	return b.String()
}
