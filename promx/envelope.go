package promx

import (
	"strconv"

	"github.com/baditaflorin/go-common/response"
	"github.com/prometheus/client_golang/prometheus"
)

// EnvelopeCollectors records response.Envelope emission for the fleet.
// Wired by AutoWire as a process-wide default observer.
//
// Metrics exposed:
//
//	fleet_envelope_emitted_total{service, schema_version}
//	fleet_envelope_warnings_total{service, reason}
//
// schema_version is the integer stringified for use as a label —
// callers bump it monotonically per service so cardinality is bounded
// (1, 2, 3, …, not arbitrary). "reason" is one of: conflict__service,
// conflict__schema_version, conflict__emitted_at, marshal_failed.
type EnvelopeCollectors struct {
	service string

	emitted  *prometheus.CounterVec
	warnings *prometheus.CounterVec
}

// NewEnvelopeCollectors registers the envelope collectors on reg. reg
// may be nil — the shared promx.Registry() is used in that case.
func NewEnvelopeCollectors(reg prometheus.Registerer) *EnvelopeCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &EnvelopeCollectors{
		service: ServiceID(),
		emitted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_envelope_emitted_total",
			Help: "Total response.Envelope() calls, labelled by schema_version (stringified integer).",
		}, []string{"service", "schema_version"}),
		warnings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_envelope_warnings_total",
			Help: "Total Envelope calls that produced a warning (reserved-key conflict, marshal failure).",
		}, []string{"service", "reason"}),
	}
	reg.MustRegister(c.emitted, c.warnings)
	return c
}

// ObserveEnvelope satisfies response.Observer.
func (c *EnvelopeCollectors) ObserveEnvelope(ev response.Event) {
	svc := c.service
	if svc == "" {
		svc = ev.Service
	}
	c.emitted.WithLabelValues(svc, strconv.Itoa(ev.SchemaVersion)).Inc()
	if ev.Warning != "" {
		c.warnings.WithLabelValues(svc, ev.Warning).Inc()
	}
}
