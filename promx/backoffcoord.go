package promx

import (
	"github.com/baditaflorin/go-common/backoffcoord"
	"github.com/prometheus/client_golang/prometheus"
)

// BackoffCoordCollectors records backoffcoord.Client.Consult outcomes.
// Wired by AutoWire as a process-wide default observer.
//
// Metrics exposed:
//
//	backoffcoord_consults_total{service, host, outcome}
//	backoffcoord_consult_latency_seconds{service}
//	backoffcoord_wait_seconds{service, host}
//
// "outcome" is one of: no_wait, waited, unreachable.
type BackoffCoordCollectors struct {
	service string
	cap     *hostCardCap

	consults *prometheus.CounterVec
	consult  *prometheus.HistogramVec
	waited   *prometheus.HistogramVec
}

// NewBackoffCoordCollectors registers the backoffcoord collectors on
// reg. reg may be nil — the shared promx.Registry() is used in that
// case.
func NewBackoffCoordCollectors(reg prometheus.Registerer) *BackoffCoordCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &BackoffCoordCollectors{
		service: ServiceID(),
		cap:     newHostCardCap(256),
		consults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "backoffcoord_consults_total",
			Help: "Total backoffcoord.Client.Consult calls, labelled by host and outcome.",
		}, []string{"service", "host", "outcome"}),
		consult: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "backoffcoord_consult_latency_seconds",
			Help:    "Wall-clock duration of the consult round-trip.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		}, []string{"service"}),
		waited: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "backoffcoord_wait_seconds",
			Help:    "Sleep duration recommended by the coordinator (capped at Client.MaxWait).",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"service", "host"}),
	}
	reg.MustRegister(c.consults, c.consult, c.waited)
	return c
}

// ObserveBackoffCoord satisfies backoffcoord.Observer.
func (c *BackoffCoordCollectors) ObserveBackoffCoord(ev backoffcoord.Event) {
	host := c.cap.label(ev.Host)
	c.consults.WithLabelValues(c.service, host, ev.Outcome).Inc()
	c.consult.WithLabelValues(c.service).Observe(ev.ConsultLatency.Seconds())
	if ev.Waited > 0 {
		c.waited.WithLabelValues(c.service, host).Observe(ev.Waited.Seconds())
	}
}
