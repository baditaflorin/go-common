package promx

import (
	"github.com/baditaflorin/go-common/apikey"
	"github.com/prometheus/client_golang/prometheus"
)

// AdminCollectors records apikey admin calls (Issue / Revoke / List /
// Purge) for the fleet. Wire by setting Client.AdminObs to the
// returned value, or call AutoWire and read promx.AutoAdmin().
//
// Metrics exposed:
//
//	apikey_admin_total{service, op, result}
//	apikey_admin_duration_seconds{service, op}
//
// "op" is one of: issue, revoke, list, purge, _other.
// "result" is one of: ok, unauthorized, unavailable, client_error,
// transport_error.
//
// Why admin calls deserve their own panel: a flurry of `unauthorized`
// is the canary for a bad/rotated admin token; a sudden spike in
// `issue` is the canary for a misbehaving caller batch-minting keys.
type AdminCollectors struct {
	service string

	total    *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewAdminCollectors registers the admin collectors on reg. reg may
// be nil — the shared promx.Registry() is used in that case.
func NewAdminCollectors(reg prometheus.Registerer) *AdminCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &AdminCollectors{
		service: ServiceID(),
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "apikey_admin_total",
			Help: "Total apikey admin calls, labelled by operation and outcome.",
		}, []string{"service", "op", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "apikey_admin_duration_seconds",
			Help:    "Wall-clock duration of apikey admin calls.",
			Buckets: prometheus.DefBuckets,
		}, []string{"service", "op"}),
	}
	reg.MustRegister(c.total, c.duration)
	return c
}

// ObserveAdmin satisfies apikey.AdminObserver.
func (c *AdminCollectors) ObserveAdmin(ev apikey.AdminEvent) {
	c.total.WithLabelValues(c.service, ev.Op, ev.Result).Inc()
	c.duration.WithLabelValues(c.service, ev.Op).Observe(ev.Duration.Seconds())
}
