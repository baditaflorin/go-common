package promx

import (
	"github.com/baditaflorin/go-common/safehttp"
	"github.com/prometheus/client_golang/prometheus"
)

// BackoffCollectors records safehttp backoff-coordinator
// consultations. Wired by AutoWire as a process-wide default
// observer.
//
// Metrics exposed:
//
//	safehttp_backoff_consults_total{service, host, outcome}
//	safehttp_backoff_consult_latency_seconds{service}
//	safehttp_backoff_waited_seconds{service, host}
//
// "outcome" is one of: consulted_no_wait, consulted_waited,
// unreachable. A sustained spike of "unreachable" is the canary for
// go-fleet-backoff-coordinator being down.
type BackoffCollectors struct {
	service string
	cap     *hostCardCap

	consults *prometheus.CounterVec
	consult  *prometheus.HistogramVec
	waited   *prometheus.HistogramVec
}

// NewBackoffCollectors registers the backoff collectors on reg. reg
// may be nil — the shared promx.Registry() is used in that case.
func NewBackoffCollectors(reg prometheus.Registerer) *BackoffCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &BackoffCollectors{
		service: ServiceID(),
		cap:     newHostCardCap(256),
		consults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "safehttp_backoff_consults_total",
			Help: "Total backoff-coordinator consultations, labelled by host and outcome.",
		}, []string{"service", "host", "outcome"}),
		consult: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "safehttp_backoff_consult_latency_seconds",
			Help:    "Wall-clock duration of the consultation HTTP round-trip.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		}, []string{"service"}),
		waited: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "safehttp_backoff_waited_seconds",
			Help:    "How long the coordinator told the caller to sleep (clamped at maxBackoffSleep).",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"service", "host"}),
	}
	reg.MustRegister(c.consults, c.consult, c.waited)
	return c
}

// ObserveBackoff satisfies safehttp.BackoffObserver.
func (c *BackoffCollectors) ObserveBackoff(ev safehttp.BackoffEvent) {
	host := c.cap.label(ev.Host)
	c.consults.WithLabelValues(c.service, host, ev.Outcome).Inc()
	c.consult.WithLabelValues(c.service).Observe(ev.ConsultLatency.Seconds())
	if ev.Waited > 0 {
		c.waited.WithLabelValues(c.service, host).Observe(ev.Waited.Seconds())
	}
}
