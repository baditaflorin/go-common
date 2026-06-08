package promx

import (
	"github.com/baditaflorin/go-common/loadshed"
	"github.com/prometheus/client_golang/prometheus"
)

// LoadshedCollectors records loadshed.Gate transitions for the fleet.
// Wired by AutoWire as a process-wide default observer, so every gate
// constructed via loadshed.New emits metrics with no per-gate wiring.
//
// Metrics exposed:
//
//	loadshed_in_flight{service, gate}      // gauge: admitted-but-not-released callers
//	loadshed_admitted_total{service, gate} // counter: callers granted a slot
//	loadshed_shed_total{service, gate}     // counter: callers refused (gate full)
//
// loadshed_shed_total is the canonical "this service is shedding load"
// alert signal — any sustained nonzero rate means the gate's upstream
// is saturated and callers are being fast-503'd. The companion
// loadshed_in_flight pinned at the gate's limit confirms saturation
// (vs. a transient blip).
type LoadshedCollectors struct {
	service string

	inflight      *prometheus.GaugeVec
	admittedTotal *prometheus.CounterVec
	shedTotal     *prometheus.CounterVec
}

// NewLoadshedCollectors registers the loadshed collectors on reg. reg
// may be nil — the shared promx.Registry() is used in that case.
func NewLoadshedCollectors(reg prometheus.Registerer) *LoadshedCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &LoadshedCollectors{
		service: ServiceID(),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loadshed_in_flight",
			Help: "Current number of admitted-but-not-released callers holding a loadshed gate slot.",
		}, []string{"service", "gate"}),
		admittedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loadshed_admitted_total",
			Help: "Total callers granted a loadshed gate slot.",
		}, []string{"service", "gate"}),
		shedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loadshed_shed_total",
			Help: "Total callers shed (fast-503'd) because a loadshed gate was full. The canonical load-shedding alert signal.",
		}, []string{"service", "gate"}),
	}
	reg.MustRegister(c.inflight, c.admittedTotal, c.shedTotal)
	return c
}

// ObserveLoadshed satisfies loadshed.Observer.
func (c *LoadshedCollectors) ObserveLoadshed(ev loadshed.Event) {
	c.inflight.WithLabelValues(c.service, ev.Gate).Set(float64(ev.InFlight))
	switch ev.Phase {
	case loadshed.PhaseAdmitted:
		c.admittedTotal.WithLabelValues(c.service, ev.Gate).Inc()
	case loadshed.PhaseShed:
		c.shedTotal.WithLabelValues(c.service, ev.Gate).Inc()
	}
}
