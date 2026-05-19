package promx

import (
	"github.com/baditaflorin/go-common/degraded"
	"github.com/prometheus/client_golang/prometheus"
)

// DegradedCollectors records degraded.Sink.Append events for the
// fleet. Wired by AutoWire as a process-wide default observer.
//
// Metrics exposed:
//
//	fleet_degraded_total{service, primitive, suffix}
//
// "primitive" is the upstream name (html-proxy, keystore, …);
// "suffix" is the per-token bucket (down, degraded, timeout,
// rate_limited). Cardinality is bounded by (services × siblings ×
// 4-ish suffixes) and stable across requests because callers reuse
// the same primitive names.
type DegradedCollectors struct {
	service string

	appends *prometheus.CounterVec
}

// NewDegradedCollectors registers the degraded-sink collector on reg.
// reg may be nil — the shared promx.Registry() is used in that case.
func NewDegradedCollectors(reg prometheus.Registerer) *DegradedCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &DegradedCollectors{
		service: ServiceID(),
		appends: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_degraded_total",
			Help: "Total degraded-sink appends, labelled by primitive and suffix (down/degraded/timeout/...).",
		}, []string{"service", "primitive", "suffix"}),
	}
	reg.MustRegister(c.appends)
	return c
}

// ObserveDegraded satisfies degraded.Observer.
func (c *DegradedCollectors) ObserveDegraded(ev degraded.Event) {
	suffix := ev.Suffix
	if suffix == "" {
		suffix = "_none"
	}
	c.appends.WithLabelValues(c.service, ev.Primitive, suffix).Inc()
}
