package promx

import (
	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

// AuthCollectors records keystore-auth observability for the fleet. A
// single AuthCollectors implements both middleware.AuthObserver (for the
// TokenAuthKeystore decision path) and apikey.CacheObserver (for the
// Cache.Verify hit/miss/stale path). Wire it on both sides:
//
//	auth := promx.NewAuthCollectors(reg)
//
//	cache := apikey.NewCache(apikey.New())
//	cache.Observer = auth
//
//	mw := middleware.TokenAuthKeystore(middleware.KeystoreOpts{
//	    Verifier: cache,
//	    Observer: auth,
//	    ...
//	})
//
// Metrics exposed:
//
//	apikey_auth_total{service, source, result}
//	apikey_keystore_call_duration_seconds{service, result}
//	apikey_cache_total{service, result}
//	apikey_cache_stale_age_seconds{service}
//	apikey_keystore_inner_duration_seconds{service, result}
//
// "source" answers operator questions like "what fraction of inbound
// traffic is already gateway-authenticated?" — the gateway label tells
// you how much keystore load is being shielded by nginx auth_request.
// "result" lets you alert on a sudden spike in "unavailable" (keystore
// outage with no cached fallback) or "stale" (caches papering over a
// real outage).
type AuthCollectors struct {
	service string

	authTotal              *prometheus.CounterVec
	keystoreCallDuration   *prometheus.HistogramVec
	cacheTotal             *prometheus.CounterVec
	cacheStaleAge          *prometheus.HistogramVec
	cacheInnerCallDuration *prometheus.HistogramVec
}

// NewAuthCollectors registers the keystore-auth collectors on reg.
// reg may be nil — the shared promx.Registry() is used in that case.
func NewAuthCollectors(reg prometheus.Registerer) *AuthCollectors {
	if reg == nil {
		reg = Registry()
	}
	service := ServiceID()

	c := &AuthCollectors{
		service: service,
		authTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "apikey_auth_total",
			Help: "Total auth decisions made by middleware.TokenAuthKeystore, labelled by the code path that produced the decision and the outcome.",
		}, []string{"service", "source", "result"}),
		keystoreCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "apikey_keystore_call_duration_seconds",
			Help:    "Duration of upstream keystore verify calls made from the auth middleware (includes cache fall-through).",
			Buckets: prometheus.DefBuckets,
		}, []string{"service", "result"}),
		cacheTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "apikey_cache_total",
			Help: "Total apikey.Cache.Verify calls, labelled by how the answer was produced (fresh, stale, inner_ok, inner_invalid, inner_unavailable).",
		}, []string{"service", "result"}),
		cacheStaleAge: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "apikey_cache_stale_age_seconds",
			Help:    "Age in seconds of cache entries served during a keystore outage (CacheResultStale). A growing distribution indicates a sustained outage.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 900, 1800},
		}, []string{"service"}),
		cacheInnerCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "apikey_keystore_inner_duration_seconds",
			Help:    "Duration of upstream verify calls observed by apikey.Cache (the keystore round-trip without cache-lookup overhead).",
			Buckets: prometheus.DefBuckets,
		}, []string{"service", "result"}),
	}
	reg.MustRegister(
		c.authTotal,
		c.keystoreCallDuration,
		c.cacheTotal,
		c.cacheStaleAge,
		c.cacheInnerCallDuration,
	)
	return c
}

// ObserveAuth satisfies middleware.AuthObserver.
func (c *AuthCollectors) ObserveAuth(ev middleware.AuthEvent) {
	c.authTotal.WithLabelValues(c.service, string(ev.Source), string(ev.Result)).Inc()
	// Only the keystore path actually does a call worth timing — the
	// bypass / gateway / local / missing paths report Duration=0 and
	// observing 0 here would skew the histogram.
	if ev.Source == middleware.AuthSourceKeystore {
		c.keystoreCallDuration.WithLabelValues(c.service, string(ev.Result)).Observe(ev.Duration.Seconds())
	}
}

// ObserveCache satisfies apikey.CacheObserver.
func (c *AuthCollectors) ObserveCache(ev apikey.CacheEvent) {
	c.cacheTotal.WithLabelValues(c.service, string(ev.Result)).Inc()
	if ev.Result == apikey.CacheResultStale && ev.Age > 0 {
		c.cacheStaleAge.WithLabelValues(c.service).Observe(ev.Age.Seconds())
	}
	if ev.Duration > 0 {
		c.cacheInnerCallDuration.WithLabelValues(c.service, string(ev.Result)).Observe(ev.Duration.Seconds())
	}
}
