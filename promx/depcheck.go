package promx

import (
	"github.com/baditaflorin/go-common/depcheck"
	"github.com/prometheus/client_golang/prometheus"
)

// DepCollectors records depcheck probe outcomes for the fleet. Wire via:
//
//	deps := depcheck.New().SetObserver(promx.NewDepCollectors(reg))
//	deps.Register("html-proxy", probe)
//
// Metrics exposed:
//
//	fleet_dep_probe_ok{service, dep}             // gauge: 1 = healthy, 0 = degraded (last probe)
//	fleet_dep_probe_runs_total{service, dep, result}
//	fleet_dep_probe_latency_seconds{service, dep}
//
// "result" is "ok" or "fail". The fleet-wide "who depends on what is
// red right now?" query is one PromQL line:
//
//	`min by (dep) (fleet_dep_probe_ok)`.
type DepCollectors struct {
	service string

	ok      *prometheus.GaugeVec
	runs    *prometheus.CounterVec
	latency *prometheus.HistogramVec
}

// NewDepCollectors registers the depcheck collectors on reg. reg may
// be nil — the shared promx.Registry() is used in that case.
func NewDepCollectors(reg prometheus.Registerer) *DepCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &DepCollectors{
		service: ServiceID(),
		ok: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fleet_dep_probe_ok",
			Help: "Last-observed health of a registered dependency probe (1 = ok, 0 = fail).",
		}, []string{"service", "dep"}),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_dep_probe_runs_total",
			Help: "Total dependency probe runs, labelled by outcome.",
		}, []string{"service", "dep", "result"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fleet_dep_probe_latency_seconds",
			Help:    "Per-probe latency (one observation per Snapshot per dep).",
			Buckets: prometheus.DefBuckets,
		}, []string{"service", "dep"}),
	}
	reg.MustRegister(c.ok, c.runs, c.latency)
	return c
}

// ObserveDep satisfies depcheck.Observer.
func (c *DepCollectors) ObserveDep(ev depcheck.Event) {
	result := "ok"
	val := 1.0
	if !ev.OK {
		result = "fail"
		val = 0
	}
	c.ok.WithLabelValues(c.service, ev.Dep).Set(val)
	c.runs.WithLabelValues(c.service, ev.Dep, result).Inc()
	c.latency.WithLabelValues(c.service, ev.Dep).Observe(ev.Latency.Seconds())
}
