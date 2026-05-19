package promx

import (
	"github.com/baditaflorin/go-common/circuitbreaker"
	"github.com/prometheus/client_golang/prometheus"
)

// CircuitCollectors records circuitbreaker transitions for the fleet.
// Wired by AutoWire as a process-wide default observer.
//
// Metrics exposed:
//
//	fleet_circuit_state{service, upstream}        // 0=closed 1=open 2=half_open
//	fleet_circuit_transitions_total{service, upstream, from, to, reason}
//	fleet_circuit_trips_total{service, upstream}  // shortcut: transitions to open
//
// fleet_circuit_state is the visual signal — one line per
// (service, upstream); a 1-line on a graph is a tripped breaker.
// transitions_total captures the full state machine for forensic
// drill-down; trips_total is the common "did this breaker trip?"
// alert target.
type CircuitCollectors struct {
	service string

	state       *prometheus.GaugeVec
	transitions *prometheus.CounterVec
	trips       *prometheus.CounterVec
}

// NewCircuitCollectors registers the circuitbreaker collectors on
// reg. reg may be nil — the shared promx.Registry() is used in that
// case.
func NewCircuitCollectors(reg prometheus.Registerer) *CircuitCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &CircuitCollectors{
		service: ServiceID(),
		state: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fleet_circuit_state",
			Help: "Current circuit breaker state (0=closed, 1=open, 2=half_open).",
		}, []string{"service", "upstream"}),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_circuit_transitions_total",
			Help: "Total circuit-breaker state transitions, labelled by from/to/reason.",
		}, []string{"service", "upstream", "from", "to", "reason"}),
		trips: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_circuit_trips_total",
			Help: "Total times a circuit-breaker transitioned to open.",
		}, []string{"service", "upstream"}),
	}
	reg.MustRegister(c.state, c.transitions, c.trips)
	return c
}

// ObserveCircuit satisfies circuitbreaker.Observer.
func (c *CircuitCollectors) ObserveCircuit(ev circuitbreaker.Event) {
	c.state.WithLabelValues(c.service, ev.Upstream).Set(float64(ev.To))
	c.transitions.WithLabelValues(c.service, ev.Upstream, ev.From.String(), ev.To.String(), ev.Reason).Inc()
	if ev.To == circuitbreaker.StateOpen && ev.From != circuitbreaker.StateOpen {
		c.trips.WithLabelValues(c.service, ev.Upstream).Inc()
	}
}
