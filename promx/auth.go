package promx

import (
	"sync"

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
//	spiffe_verify_failures_total{service, source, reason}
//	spiffe_apikey_fallback_total{service, principal, reason}
//
// The last two are the SPIFFE dual-mode counters (package spiffeauth); they
// have no series until a service sets SPIFFEAUTH_MODE. "source" on
// apikey_auth_total also takes "spiffe-mtls" and "spiffe-jwt".
//
// "source" answers operator questions like "what fraction of inbound
// traffic is already gateway-authenticated?" — the gateway label tells
// you how much keystore load is being shielded by nginx auth_request.
// "result" lets you alert on a sudden spike in "unavailable" (keystore
// outage with no cached fallback) or "stale" (caches papering over a
// real outage). Since this is generic on ev.Result's string value, a
// service that sets KeystoreOpts.RequiredTier gets "deny-tier" (enforced)
// and "shadow-deny-tier" (shadow mode) label values on apikey_auth_total
// for free — no new metric needed to watch a tier rollout's shadow-mode
// would-be-denied rate before flipping TierEnforce.
type AuthCollectors struct {
	service string

	authTotal              *prometheus.CounterVec
	keystoreCallDuration   *prometheus.HistogramVec
	cacheTotal             *prometheus.CounterVec
	cacheStaleAge          *prometheus.HistogramVec
	cacheInnerCallDuration *prometheus.HistogramVec

	spiffeFailures *prometheus.CounterVec
	spiffeFallback *prometheus.CounterVec

	// A pointer, so AuthCollectors stays comparable (a mutex and a map in it would change its API).
	principals *principalSet
}

type principalSet struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// spiffeFailureReasons are the values of spiffe_verify_failures_total{reason}
// (the spiffeauth.Reason* tokens); anything else is counted as "other" so a
// caller of ObserveIdentityFailure cannot grow the label set.
var spiffeFailureReasons = map[string]struct{}{
	"invalid_svid": {}, "untrusted_domain": {}, "expired_token": {}, "wrong_audience": {},
	"invalid_token": {}, "ttl_too_long": {}, "unmapped_id": {}, "not_ready": {}, "other": {},
}

// maxFallbackPrincipals bounds the principal label of spiffe_apikey_fallback_total.
// The values come from the operator's peer map (at most 1024 entries), never from a request;
// the fetch-cache chain has 354 callers, so the cap sits above that.
const maxFallbackPrincipals = 512

// NewAuthCollectors registers the keystore-auth collectors on reg.
// reg may be nil — the shared promx.Registry() is used in that case.
func NewAuthCollectors(reg prometheus.Registerer) *AuthCollectors {
	if reg == nil {
		reg = Registry()
	}
	service := ServiceID()

	c := &AuthCollectors{
		service:    service,
		principals: &principalSet{seen: map[string]struct{}{}},
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
	c.spiffeFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spiffe_verify_failures_total",
		Help: "SPIFFE identities presented to the auth middleware and not accepted, by source (spiffe-mtls, spiffe-jwt) and reason.",
	}, []string{"service", "source", "reason"})
	c.spiffeFallback = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spiffe_apikey_fallback_total",
		Help: "Requests authenticated with an API key although the principal has a SPIFFE mapping (SPIFFE expected, not used), by reason (no_svid or the failure reason).",
	}, []string{"service", "principal", "reason"})
	reg.MustRegister(
		c.spiffeFailures,
		c.spiffeFallback,
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

// ObserveIdentityFailure satisfies middleware.IdentityObserver.
func (c *AuthCollectors) ObserveIdentityFailure(source middleware.AuthSource, reason string) {
	if _, ok := spiffeFailureReasons[reason]; !ok {
		reason = "other"
	}
	if source != middleware.AuthSourceSPIFFEMTLS && source != middleware.AuthSourceSPIFFEJWT {
		source = "other"
	}
	c.spiffeFailures.WithLabelValues(c.service, string(source), reason).Inc()
}

// ObserveIdentityFallback satisfies middleware.IdentityObserver.
func (c *AuthCollectors) ObserveIdentityFallback(principal, reason string) {
	if _, ok := spiffeFailureReasons[reason]; !ok && reason != "no_svid" {
		reason = "other"
	}
	if !principalLabel(principal) {
		principal = "_other"
	}
	c.principals.mu.Lock()
	if _, seen := c.principals.seen[principal]; !seen {
		if len(c.principals.seen) >= maxFallbackPrincipals {
			principal = "_other"
		} else {
			c.principals.seen[principal] = struct{}{}
		}
	}
	c.principals.mu.Unlock()
	c.spiffeFallback.WithLabelValues(c.service, principal, reason).Inc()
}

// principalLabel: the shape a peer-map principal has (spiffeauth accepts only 1-64 of
// a-z 0-9 . _ -); anything else is not a configured principal and is counted as "_other".
func principalLabel(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
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
