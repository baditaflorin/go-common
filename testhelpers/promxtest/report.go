package promxtest

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric is what one metric family costs Prometheus, read from a Gather.
type Metric struct {
	// Name is the family name, as scraped.
	Name string
	// Type is "counter", "gauge", "histogram", "summary" or "untyped".
	Type string
	// Series is how many series Prometheus stores for it: one per label combination
	// that exists, and for a histogram one per bucket plus +Inf, _sum and _count,
	// for a summary one per quantile plus _sum and _count.
	Series int
	// Children is the number of label combinations that exist.
	Children int
	// PerChild is the series one label combination costs: 1, or more for a
	// histogram or summary.
	PerChild int
	// Values holds, per label name, the sorted distinct values the label took.
	Values map[string][]string
	// MaxValueLen is the longest label value, in bytes.
	MaxValueLen int
}

// Labels returns the label names of the family, sorted.
func (m Metric) Labels() []string {
	names := make([]string, 0, len(m.Values))
	for name := range m.Values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Distinct returns how many distinct values label took (0 when it has no such label).
func (m Metric) Distinct(label string) int { return len(m.Values[label]) }

// Ceiling is the most series the family can hold if every label combination of
// the values seen can occur: the product of Distinct over its labels, times
// PerChild. It is an upper bound on what the labels, as the probes exercised them,
// can ever create, and so the number to put a budget against: Series only says
// what the probes happened to reach, and a label with ten values next to one with
// five hundred reaches 5,000 combinations only if something sends all of them.
// The product saturates at math.MaxInt32 rather than overflowing.
func (m Metric) Ceiling() int {
	n := m.PerChild
	for _, vs := range m.Values {
		if n > math.MaxInt32/max(len(vs), 1) {
			return math.MaxInt32
		}
		n *= max(len(vs), 1)
	}
	return n
}

// Report is what a registry costs Prometheus, per metric family.
type Report struct {
	// Metrics holds one entry per metric family that has at least one series,
	// sorted by name.
	Metrics []Metric
	// Total is the sum of Series.
	Total int
}

// Metric returns the family called name.
func (r Report) Metric(name string) (Metric, bool) {
	i := sort.Search(len(r.Metrics), func(i int) bool { return r.Metrics[i].Name >= name })
	if i < len(r.Metrics) && r.Metrics[i].Name == name {
		return r.Metrics[i], true
	}
	return Metric{}, false
}

// Ceiling is the sum of Metric.Ceiling.
func (r Report) Ceiling() int {
	n := 0
	for _, m := range r.Metrics {
		n += m.Ceiling()
		if n > math.MaxInt32 {
			return math.MaxInt32
		}
	}
	return n
}

// Largest returns up to n families, the ones with the most series first.
func (r Report) Largest(n int) []Metric {
	ms := append([]Metric(nil), r.Metrics...)
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Series > ms[j].Series })
	return ms[:min(n, len(ms))]
}

// String is a table of the families, largest first: series, ceiling, and the
// distinct values per label.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d series in %d families (ceiling %d)", r.Total, len(r.Metrics), r.Ceiling())
	for _, m := range r.Largest(len(r.Metrics)) {
		fmt.Fprintf(&b, "\n  %-58s %8d series  ceiling %9d ", m.Name, m.Series, m.Ceiling())
		for i, l := range m.Labels() {
			if i == 0 {
				b.WriteString(" ")
			} else {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%d", l, m.Distinct(l))
		}
	}
	return b.String()
}

// Measure gathers g and counts what Prometheus would store for it. A Gather error
// fails the test (a metric that cannot be gathered cannot be scraped either).
func Measure(t testing.TB, g prometheus.Gatherer) Report {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var r Report
	for _, mf := range mfs {
		if len(mf.GetMetric()) == 0 {
			continue
		}
		m := Metric{Name: mf.GetName(), Type: strings.ToLower(mf.GetType().String()), PerChild: 1, Values: map[string][]string{}}
		sets := map[string]map[string]struct{}{}
		for _, c := range mf.GetMetric() {
			m.Children++
			switch {
			case c.GetHistogram() != nil:
				b := c.GetHistogram().GetBucket()
				m.PerChild = len(b) + 2 // _sum and _count
				if len(b) == 0 || !math.IsInf(b[len(b)-1].GetUpperBound(), 1) {
					m.PerChild++ // the +Inf bucket the exposition adds
				}
			case c.GetSummary() != nil:
				m.PerChild = len(c.GetSummary().GetQuantile()) + 2
			}
			for _, lp := range c.GetLabel() {
				if sets[lp.GetName()] == nil {
					sets[lp.GetName()] = map[string]struct{}{}
				}
				sets[lp.GetName()][lp.GetValue()] = struct{}{}
				m.MaxValueLen = max(m.MaxValueLen, len(lp.GetValue()))
			}
		}
		for name, set := range sets {
			vs := make([]string, 0, len(set))
			for v := range set {
				vs = append(vs, v)
			}
			sort.Strings(vs)
			m.Values[name] = vs
		}
		m.Series = m.Children * m.PerChild
		r.Total += m.Series
		r.Metrics = append(r.Metrics, m)
	}
	sort.Slice(r.Metrics, func(i, j int) bool { return r.Metrics[i].Name < r.Metrics[j].Name })
	return r
}
