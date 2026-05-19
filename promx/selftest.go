package promx

import (
	"github.com/baditaflorin/go-common/selftest"
	"github.com/prometheus/client_golang/prometheus"
)

// SelftestCollectors records /selftest outcomes for the fleet. It
// implements selftest.Observer and is wired via:
//
//	st := selftest.NewSuite(id, ver, selftest.WithObserver(promx.NewSelftestCollectors(reg)))
//
// Metrics exposed:
//
//	fleet_selftest_check_pass{service, check}     // gauge: 1 = pass, 0 = fail (last run)
//	fleet_selftest_check_runs_total{service, check, result}
//	fleet_selftest_runs_total{service, result}
//	fleet_selftest_run_duration_seconds{service}
//
// "result" is one of: "pass", "fail". The gauge is the easy panel
// signal ("which checks are red right now?"); the counter pair lets
// operators alert on flap rate ("/selftest passed but the dns probe
// failed 3 times in the last minute").
//
// Cardinality: bounded by (services × distinct check names). Services
// register a handful of checks each; the cap is fleet-wide stable.
type SelftestCollectors struct {
	service string

	checkPass    *prometheus.GaugeVec
	checkRuns    *prometheus.CounterVec
	runs         *prometheus.CounterVec
	runDuration  *prometheus.HistogramVec
}

// NewSelftestCollectors registers the /selftest collectors on reg.
// reg may be nil — the shared promx.Registry() is used in that case.
func NewSelftestCollectors(reg prometheus.Registerer) *SelftestCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &SelftestCollectors{
		service: ServiceID(),
		checkPass: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fleet_selftest_check_pass",
			Help: "Last-observed pass/fail state of a named /selftest check (1 = pass, 0 = fail).",
		}, []string{"service", "check"}),
		checkRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_selftest_check_runs_total",
			Help: "Total runs of a named /selftest check, labelled by outcome.",
		}, []string{"service", "check", "result"}),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_selftest_runs_total",
			Help: "Total /selftest renders, labelled by overall outcome.",
		}, []string{"service", "result"}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fleet_selftest_run_duration_seconds",
			Help:    "Wall time of a full /selftest render (all checks combined).",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"service"}),
	}
	reg.MustRegister(c.checkPass, c.checkRuns, c.runs, c.runDuration)
	return c
}

// ObserveSelftest satisfies selftest.Observer.
func (c *SelftestCollectors) ObserveSelftest(ev selftest.Event) {
	svc := c.service
	if svc == "" {
		svc = ev.Service
	}
	overall := "pass"
	if !ev.OK {
		overall = "fail"
	}
	c.runs.WithLabelValues(svc, overall).Inc()
	c.runDuration.WithLabelValues(svc).Observe(ev.Duration.Seconds())
	for _, ck := range ev.Checks {
		result := "pass"
		val := 1.0
		if !ck.Pass {
			result = "fail"
			val = 0
		}
		c.checkPass.WithLabelValues(svc, ck.Name).Set(val)
		c.checkRuns.WithLabelValues(svc, ck.Name, result).Inc()
	}
}
