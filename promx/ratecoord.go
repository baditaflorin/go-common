package promx

import (
	"github.com/baditaflorin/go-common/ratecoord"
	"github.com/prometheus/client_golang/prometheus"
)

// RateCoordCollectors records rate-coordinator decisions for the
// fleet. Wire via:
//
//	rc := ratecoord.New().SetObserver(promx.NewRateCoordCollectors(reg))
//
// Metrics exposed:
//
//	ratecoord_decisions_total{service, host, outcome}
//	ratecoord_fallback_total{service}
//	ratecoord_wait_seconds{service, fellback}
//
// "outcome" is one of: "allowed", "fallback_allowed", "fallback_denied".
// `ratecoord_fallback_total` is a fleet-wide canary for coordinator
// outage — a sudden non-zero rate across many services means the
// central service is unreachable and every caller is now running on
// its private bucket.
//
// Host cardinality is bounded by the standard host_cap (256 by default,
// hosts beyond fold to "_other").
type RateCoordCollectors struct {
	service string
	cap     *hostCardCap

	decisions *prometheus.CounterVec
	fallback  *prometheus.CounterVec
	wait      *prometheus.HistogramVec
}

// NewRateCoordCollectors registers the ratecoord collectors on reg.
// reg may be nil — the shared promx.Registry() is used in that case.
func NewRateCoordCollectors(reg prometheus.Registerer) *RateCoordCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &RateCoordCollectors{
		service: ServiceID(),
		cap:     newHostCardCap(256),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ratecoord_decisions_total",
			Help: "Total ratecoord.Client.Wait calls, labelled by host and outcome.",
		}, []string{"service", "host", "outcome"}),
		fallback: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ratecoord_fallback_total",
			Help: "Total Wait calls that fell back to the in-process bucket (coordinator unreachable).",
		}, []string{"service"}),
		wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ratecoord_wait_seconds",
			Help:    "Time spent waiting on a token, labelled by whether the in-process fallback served the answer.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"service", "fellback"}),
	}
	reg.MustRegister(c.decisions, c.fallback, c.wait)
	return c
}

// ObserveRate satisfies ratecoord.Observer.
func (c *RateCoordCollectors) ObserveRate(ev ratecoord.Event) {
	host := c.cap.label(ev.Host)
	outcome := "allowed"
	switch {
	case ev.FellBack && !ev.Allowed:
		outcome = "fallback_denied"
	case ev.FellBack:
		outcome = "fallback_allowed"
	}
	c.decisions.WithLabelValues(c.service, host, outcome).Inc()
	if ev.FellBack {
		c.fallback.WithLabelValues(c.service).Inc()
	}
	fb := "false"
	if ev.FellBack {
		fb = "true"
	}
	c.wait.WithLabelValues(c.service, fb).Observe(ev.Waited.Seconds())
}
