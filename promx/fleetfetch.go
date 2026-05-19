package promx

import (
	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/prometheus/client_golang/prometheus"
)

// FleetFetchCollectors records fleetfetch.Client.Get outcomes. Wired
// by AutoWire as a process-wide default observer.
//
// Metrics exposed:
//
//	fleet_fetch_total{service, host, result}
//	fleet_fetch_duration_seconds{service, result}
//	fleet_fetch_cache_age_seconds{service, host}
//
// "result" is one of: hit, miss, fallback, error. Cache hit rate per
// service is one PromQL line:
//
//	`sum(rate(fleet_fetch_total{result="hit"}[5m]))
//	 /
//	 sum(rate(fleet_fetch_total[5m]))`.
//
// fallback rate is the canary for cache outages.
type FleetFetchCollectors struct {
	service string
	cap     *hostCardCap

	total    *prometheus.CounterVec
	duration *prometheus.HistogramVec
	cacheAge *prometheus.HistogramVec
}

// NewFleetFetchCollectors registers the fleetfetch collectors on reg.
// reg may be nil — the shared promx.Registry() is used in that case.
func NewFleetFetchCollectors(reg prometheus.Registerer) *FleetFetchCollectors {
	if reg == nil {
		reg = Registry()
	}
	c := &FleetFetchCollectors{
		service: ServiceID(),
		cap:     newHostCardCap(256),
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fleet_fetch_total",
			Help: "Total fleetfetch.Client.Get calls, labelled by upstream host and outcome.",
		}, []string{"service", "host", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fleet_fetch_duration_seconds",
			Help:    "Wall-clock duration of a Client.Get, labelled by outcome (hit/miss/fallback/error).",
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"service", "result"}),
		cacheAge: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fleet_fetch_cache_age_seconds",
			Help:    "Age in seconds of the cache entry served on a hit. Long ages = warm cache; growing = cache TTLs are loose.",
			Buckets: []float64{1, 10, 60, 300, 900, 3600, 21600, 86400},
		}, []string{"service", "host"}),
	}
	reg.MustRegister(c.total, c.duration, c.cacheAge)
	return c
}

// ObserveFleetFetch satisfies fleetfetch.Observer.
func (c *FleetFetchCollectors) ObserveFleetFetch(ev fleetfetch.Event) {
	host := c.cap.label(ev.Host)
	c.total.WithLabelValues(c.service, host, ev.Result).Inc()
	c.duration.WithLabelValues(c.service, ev.Result).Observe(ev.Duration.Seconds())
	if ev.Result == "hit" && ev.AgeSeconds > 0 {
		c.cacheAge.WithLabelValues(c.service, host).Observe(float64(ev.AgeSeconds))
	}
}
