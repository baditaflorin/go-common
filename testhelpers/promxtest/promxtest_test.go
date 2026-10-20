package promxtest

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
)

// recorder is a testing.TB that records failures instead of failing, so the tests
// can assert what AssertBounded says about a collector it should reject.
type recorder struct {
	testing.TB // the methods AssertBounded does not call; nil, so calling one panics
	errors     []string
	fatal      bool
}

type stopped struct{}

func (r *recorder) Helper()             {}
func (r *recorder) Logf(string, ...any) {}
func (r *recorder) Errorf(f string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(f, args...))
}
func (r *recorder) Fatalf(f string, args ...any) {
	r.Errorf(f, args...)
	r.fatal = true
	panic(stopped{})
}
func (r *recorder) failed() string { return strings.Join(r.errors, "\n") }
func (r *recorder) assert(f func(testing.TB)) (out *recorder) {
	out = r
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(stopped); !ok {
				panic(p)
			}
		}
	}()
	f(r)
	return r
}

// collector is a stand-in for a promx collector: a counter with one label that
// takes whatever string fold returns, and a histogram per label value.
type collector struct {
	fold     func(string) string
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
}

func newCollector(mk func() func(string) string) func(prometheus.Registerer) *collector {
	return func(reg prometheus.Registerer) *collector {
		c := &collector{
			fold:     mk(),
			requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "fake_requests_total"}, []string{"service", "host"}),
			latency:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "fake_latency_seconds", Buckets: []float64{0.1, 1}}, []string{"host"}),
		}
		reg.MustRegister(c.requests, c.latency)
		return c
	}
}

func (c *collector) observe(host string) {
	v := c.fold(host)
	c.requests.WithLabelValues("svc", v).Inc()
	c.latency.WithLabelValues(v).Observe(0.5)
}

var hostProbe = []Probe[*collector]{{
	Name:  "hosts",
	Drive: func(c *collector, in *Inputs) { in.Each(in.Hosts(), c.observe) },
}}

// firstN names the first n usable values it sees and folds the rest, which is what
// promx.BoundedLabel does (this test package cannot import promx: promx's own tests
// import this one).
func firstN(n int) func() func(string) string {
	return func() func(string) string {
		seen := map[string]bool{}
		return func(v string) string {
			if len(v) > DefaultMaxValueLen || !utf8.ValidString(v) {
				return "_other"
			}
			if seen[v] {
				return v
			}
			if len(seen) < n {
				seen[v] = true
				return v
			}
			return "_other"
		}
	}
}

// pass returns the value unchanged, or repaired so that client_golang accepts it.
func pass(repair bool) func() func(string) string {
	return func() func(string) string {
		return func(v string) string {
			if repair {
				return strings.ToValidUTF8(v, "?")
			}
			return v
		}
	}
}

func TestAssertBoundedAcceptsABoundedCollector(t *testing.T) {
	rep := AssertBounded(t, newCollector(firstN(10)), hostProbe, 100)
	// 11 host values (10 and _other); a counter child, and a histogram child of 2
	// buckets, +Inf, _sum and _count.
	if got, want := rep.Total, 11+11*5; got != want {
		t.Errorf("Total = %d, want %d\n%s", got, want, rep)
	}
	m, ok := rep.Metric("fake_latency_seconds")
	if !ok || m.Distinct("host") != 11 || m.PerChild != 5 || m.Ceiling() != 55 || m.Type != "histogram" {
		t.Errorf("fake_latency_seconds = %+v (ok %v)", m, ok)
	}
}

func TestAssertBoundedFailsOnAnUnboundedLabel(t *testing.T) {
	// Unbounded, but safe for client_golang: only the UTF-8 is repaired.
	raw := newCollector(pass(true))
	r := new(recorder).assert(func(tb testing.TB) { AssertBounded(tb, raw, hostProbe, 1_000_000, WithMaxValueLen(1<<30)) })
	msg := r.failed()
	if !strings.Contains(msg, "fake_requests_total is not bounded") || !strings.Contains(msg, "host (") {
		t.Errorf("want the unbounded counter and its label named, got:\n%s", msg)
	}
	if !strings.Contains(msg, "fake_latency_seconds is not bounded") {
		t.Errorf("want the unbounded histogram named too, got:\n%s", msg)
	}
	if r.fatal {
		t.Errorf("unbounded is an error, not a fatal one: the report should still be produced")
	}
}

func TestAssertBoundedFailsOverBudget(t *testing.T) {
	r := new(recorder).assert(func(tb testing.TB) { AssertBounded(tb, newCollector(firstN(10)), hostProbe, 50) })
	if msg := r.failed(); !strings.Contains(msg, "66 series, budget 50") || !strings.Contains(msg, "fake_latency_seconds") {
		t.Errorf("want the budget failure with the largest family named, got:\n%s", msg)
	}
}

func TestAssertBoundedReportsAPanicAndGoesOn(t *testing.T) {
	// client_golang panics on a label value that is not valid UTF-8.
	raw := newCollector(pass(false))
	r := new(recorder).assert(func(tb testing.TB) { AssertBounded(tb, raw, hostProbe, 1_000_000) })
	if msg := r.failed(); !strings.Contains(msg, `probe "hosts" panicked`) || !strings.Contains(msg, "not valid UTF-8") {
		t.Errorf("want the panic reported with its cause, got:\n%s", msg)
	}
}

func TestAssertBoundedFailsOnALongValue(t *testing.T) {
	long := newCollector(func() func(string) string {
		return func(v string) string { return strings.Repeat("a", 300) + fmt.Sprint(len(v)%3) }
	})
	r := new(recorder).assert(func(tb testing.TB) { AssertBounded(tb, long, hostProbe, 1000) })
	if msg := r.failed(); !strings.Contains(msg, "label value of 301 bytes, limit 256") {
		t.Errorf("want the long value reported, got:\n%s", msg)
	}
	// The limit is the caller's to move.
	ok := new(recorder).assert(func(tb testing.TB) { AssertBounded(tb, long, hostProbe, 1000, WithMaxValueLen(301)) })
	if msg := ok.failed(); msg != "" {
		t.Errorf("a raised limit should pass, got:\n%s", msg)
	}
}

func TestAssertBoundedFailsWhenTheProbesReachNothing(t *testing.T) {
	idle := []Probe[*collector]{{Name: "idle", Drive: func(*collector, *Inputs) {}}}
	r := new(recorder).assert(func(tb testing.TB) { AssertBounded(tb, newCollector(firstN(1)), idle, 100) })
	if !r.fatal || !strings.Contains(r.failed(), "created no series") {
		t.Errorf("want a fatal failure that says nothing was measured, got fatal=%v:\n%s", r.fatal, r.failed())
	}
}

// TestRunSharesARegistry: Run puts several collector sets in one registry, which
// is how a package-wide budget test measures everything together.
func TestRunSharesARegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	in := NewInputs(1)
	Run(t, reg, newCollector(firstN(3)), hostProbe, in)
	Run(t, reg, func(r prometheus.Registerer) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{Name: "other_total"})
		r.MustRegister(c)
		return c
	}, []Probe[prometheus.Counter]{{Name: "inc", Drive: func(c prometheus.Counter, _ *Inputs) { c.Inc() }}}, in)
	rep := Measure(t, reg)
	if _, ok := rep.Metric("other_total"); !ok || rep.Total != 4+4*5+1 {
		t.Errorf("want both collector sets in one report (%d series), got:\n%s", 4+4*5+1, rep)
	}
}

func TestMeasureCountsSeriesTheWayPrometheusStoresThem(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "h_seconds", Buckets: []float64{1, 2, 3}}, []string{"a", "b"})
	h.WithLabelValues("x", "1").Observe(1)
	h.WithLabelValues("x", "2").Observe(1)
	h.WithLabelValues("y", "1").Observe(1)
	s := prometheus.NewSummary(prometheus.SummaryOpts{Name: "s_seconds", Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01}})
	s.Observe(1)
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "g", ConstLabels: prometheus.Labels{"service": "svc"}})
	reg.MustRegister(h, s, g)

	rep := Measure(t, reg)
	hm, _ := rep.Metric("h_seconds")
	if hm.Series != 3*(3+3) || hm.Children != 3 || hm.Distinct("a") != 2 || hm.Distinct("b") != 2 {
		t.Errorf("histogram: %+v", hm)
	}
	// Four combinations are possible from 2 x 2 values, three exist.
	if hm.Ceiling() != 4*(3+3) {
		t.Errorf("histogram ceiling = %d, want %d", hm.Ceiling(), 4*6)
	}
	if sm, _ := rep.Metric("s_seconds"); sm.Series != 2+2 || sm.Type != "summary" {
		t.Errorf("summary: %+v", sm)
	}
	if gm, _ := rep.Metric("g"); gm.Series != 1 || gm.Ceiling() != 1 || gm.Distinct("service") != 1 {
		t.Errorf("gauge: %+v", gm)
	}
	if rep.Total != 18+4+1 || rep.Ceiling() != 24+4+1 {
		t.Errorf("Total %d, Ceiling %d, want 23 and 29", rep.Total, rep.Ceiling())
	}
	if _, ok := rep.Metric("nope"); ok {
		t.Errorf("Metric(nope) found something")
	}
	if got := rep.Largest(1); len(got) != 1 || got[0].Name != "h_seconds" {
		t.Errorf("Largest(1) = %v", got)
	}
}

func TestCeilingSaturates(t *testing.T) {
	vals := make([]string, 70000)
	for i := range vals {
		vals[i] = fmt.Sprint(i)
	}
	m := Metric{PerChild: 14, Values: map[string][]string{"a": vals, "b": vals, "c": vals}}
	if got := m.Ceiling(); got <= 0 {
		t.Errorf("Ceiling overflowed to %d", got)
	}
}

func TestInputsAreDistinctDeterministicAndHostile(t *testing.T) {
	for _, scale := range []int{1, 4} {
		in, again := NewInputs(scale), NewInputs(scale)
		pools := func(in *Inputs) map[string][]string {
			return map[string][]string{
				"hosts": in.Hosts(), "methods": in.Methods(), "paths": in.Paths(),
				"schemes": in.Schemes(), "useragents": in.UserAgents(), "names": in.Names(),
			}
		}
		other := pools(again)
		for name, p := range pools(in) {
			if len(p) != 2000*scale {
				t.Errorf("scale %d: %s has %d values, want %d", scale, name, len(p), 2000*scale)
			}
			seen := map[string]bool{}
			var invalid, long, huge, unicode, ctrl int
			for _, v := range p {
				if seen[v] {
					t.Fatalf("scale %d: %s repeats %.30q", scale, name, v)
				}
				seen[v] = true
				switch {
				case !utf8.ValidString(v):
					invalid++
				case len(v) > 1<<20:
					huge++
				case len(v) > 256:
					long++
				}
				if strings.ContainsRune(v, '日') {
					unicode++
				}
				if strings.ContainsAny(v, "\x00\r\n\"") {
					ctrl++
				}
			}
			if p[0] != "" || invalid == 0 || long == 0 || huge == 0 || unicode == 0 || ctrl == 0 {
				t.Errorf("scale %d: %s lacks a hostile shape: empty first %v, invalid %d, long %d, huge %d, unicode %d, control %d", scale, name, p[0] == "", invalid, long, huge, unicode, ctrl)
			}
			if !slices.Equal(p, other[name]) {
				t.Errorf("scale %d: a second Inputs built a different %s pool", scale, name)
			}
		}
	}
	// The standard methods and the two schemes that matter are in the pool as they are.
	in := NewInputs(1)
	for _, want := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE"} {
		if !slices.Contains(in.Methods(), want) {
			t.Errorf("Methods lacks %s", want)
		}
	}
	for _, want := range []string{"http", "https"} {
		if !slices.Contains(in.Schemes(), want) {
			t.Errorf("Schemes lacks %s", want)
		}
	}
	if s := in.Statuses(); len(s) != 900 || s[0] != 100 || s[899] != 999 {
		t.Errorf("Statuses: %d values from %d to %d, want 900 from 100 to 999", len(s), s[0], s[len(s)-1])
	}
}

func TestEachFloodsThenBursts(t *testing.T) {
	in := &Inputs{Burst: 2, Repeat: 3}
	var got []string
	in.Each([]string{"a", "b", "c"}, func(v string) { got = append(got, v) })
	if want := "a b c a a a b b b"; strings.Join(got, " ") != want {
		t.Errorf("Each visited %q, want %q", strings.Join(got, " "), want)
	}
}
