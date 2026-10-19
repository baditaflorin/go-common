package promx

import (
	"errors"
	"strings"

	"github.com/baditaflorin/go-common/safehttp"
	"github.com/prometheus/client_golang/prometheus"
)

// EgressCollectors records every outbound HTTP attempt made through a
// safehttp client configured with safehttp.WithObserver(c). It implements
// safehttp.EgressObserver.
//
// Metrics exposed (labels in order):
//
//	safehttp_egress_requests_total{service, host, scheme, via_proxy, outcome}
//	safehttp_egress_duration_seconds{service, via_proxy}
//	safehttp_egress_response_bytes_total{service, host, channel}
//	safehttp_egress_blocked_total{service, reason}
//
// "host" is bounded per process: at most 32 hosts get their own value (see
// WithHostLimit), and a host earns one only once it has made 64 requests
// (see WithHostMinHits). Every other request counts under host="_other",
// so a crawler's long tail of domains costs one series instead of one per
// domain, while the upstreams it calls all the time keep their names; sums
// over "host" stay exact. The duration histogram carries no "host" at all:
// a histogram costs a series per bucket, and per-host latency was the
// largest single cost in the fleet's Prometheus (611k of 1.42M head series
// on 2026-10-19). "outcome" is a fixed-size enum drawn from
// safehttp.EgressOutcome (~9 values), so the product label space is
// bounded.
//
// "via_proxy" is "true" / "false". Pair the requests_total counter with
// safehttp_egress_blocked_total (which fires before the proxy is even
// consulted) to answer the operator question "which services are leaking
// direct egress past the proxy?":
//
//	sum by (service) (
//	  rate(safehttp_egress_requests_total{via_proxy="false"}[5m])
//	)
//
// "channel" (bytesTotal only — via_proxy already covers direct/proxy on the
// other two vectors, and stacking both would double cardinality for no new
// information) is "direct" / "proxy" / "cache_hit", from safehttp.EgressEvent.
// Use it to separate real network spend from a fetch-cache hit:
//
//	sum by (service) (
//	  rate(safehttp_egress_response_bytes_total{channel!="cache_hit"}[5m])
//	)
type EgressCollectors struct {
	service string

	requestsTotal *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	bytesTotal    *prometheus.CounterVec
	blockedTotal  *prometheus.CounterVec

	hosts *hostCardCap
}

// EgressOption configures NewEgressCollectors.
type EgressOption func(*egressOpts)

type egressOpts struct {
	hostLimit   int
	hostMinHits int
	buckets     []float64
}

// WithHostLimit caps the number of hosts that get their own "host" label
// value; every other request counts under "_other". Admitted hosts keep
// their value for the life of the process, so the label never takes more
// than n+2 values ("_other", and "_unknown" for an event with no host).
// Default: 32 (256 before 0.105.0); n <= 0 keeps the default.
func WithHostLimit(n int) EgressOption {
	return func(o *egressOpts) { o.hostLimit = n }
}

// WithHostMinHits sets how many requests a host must make before it gets
// its own "host" label value; until then its requests count under
// "_other". The count has to be reached before 1024 other not-yet-admitted
// hosts appear, so a crawler's one-off domains never qualify and the
// upstreams it calls for every domain do. Default: 64. n <= 1 restores the
// rule from before 0.105.0: the first hosts seen take the slots.
func WithHostMinHits(n int) EgressOption {
	return func(o *egressOpts) { o.hostMinHits = n }
}

// WithDurationBuckets overrides the default histogram buckets. The
// default (prometheus.DefBuckets, 5ms…10s) is the same set every fleet
// service has been using ad-hoc; override only if you actually need
// different SLO targets.
func WithDurationBuckets(b []float64) EgressOption {
	return func(o *egressOpts) { o.buckets = b }
}

// NewEgressCollectors registers the egress collectors on reg and returns
// an observer. Pass it to safehttp:
//
//	reg := promx.Init("my-service", Version)
//	obs := promx.NewEgressCollectors(reg)
//	client := safehttp.NewClient(safehttp.WithObserver(obs), ...)
//
// reg can be nil, in which case the shared promx.Registry() is used.
func NewEgressCollectors(reg prometheus.Registerer, opts ...EgressOption) *EgressCollectors {
	o := &egressOpts{hostLimit: defaultHostLimit, hostMinHits: defaultHostMinHits, buckets: prometheus.DefBuckets}
	for _, opt := range opts {
		opt(o)
	}
	if o.hostLimit <= 0 {
		o.hostLimit = defaultHostLimit
	}
	if reg == nil {
		reg = Registry()
	}
	service := ServiceID()

	c := &EgressCollectors{
		service: service,
		hosts:   newHeavyHitterCap(o.hostLimit, o.hostMinHits, 0),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "safehttp_egress_requests_total",
			Help: "Total outbound HTTP requests made via safehttp clients, labelled by destination host, scheme, proxy usage, and outcome bucket.",
		}, []string{"service", "host", "scheme", "via_proxy", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "safehttp_egress_duration_seconds",
			Help:    "Outbound HTTP request duration in seconds (full round-trip including TLS + body fetch).",
			Buckets: o.buckets,
		}, []string{"service", "via_proxy"}),
		bytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "safehttp_egress_response_bytes_total",
			Help: "Total response bytes actually read per outbound host (falls back to Content-Length only when the caller closed the body unread), labeled by channel (direct/proxy/cache_hit) so a cache hit doesn't look like real network spend.",
		}, []string{"service", "host", "channel"}),
		blockedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "safehttp_egress_blocked_total",
			Help: "Total outbound requests rejected by safehttp guards (SSRF, scheme, port).",
		}, []string{"service", "reason"}),
	}
	reg.MustRegister(c.requestsTotal, c.duration, c.bytesTotal, c.blockedTotal)
	return c
}

// ObserveEgress satisfies safehttp.EgressObserver.
func (c *EgressCollectors) ObserveEgress(ev safehttp.EgressEvent) {
	host := c.hosts.label(ev.Host)
	viaProxy := boolLabel(ev.ViaProxy)

	c.requestsTotal.WithLabelValues(c.service, host, ev.Scheme, viaProxy, string(ev.Outcome)).Inc()
	c.duration.WithLabelValues(c.service, viaProxy).Observe(ev.Duration.Seconds())
	if ev.Bytes > 0 {
		channel := ev.Channel
		if channel == "" {
			// Events from before the Channel field existed, or a caller
			// hand-building an EgressEvent — fall back to via_proxy so the
			// label is never empty (empty would still be counted correctly,
			// just harder to query).
			channel = "direct"
			if ev.ViaProxy {
				channel = "proxy"
			}
		}
		c.bytesTotal.WithLabelValues(c.service, host, channel).Add(float64(ev.Bytes))
	}
	if ev.Outcome == safehttp.OutcomeBlocked {
		c.blockedTotal.WithLabelValues(c.service, blockReason(ev)).Inc()
	}
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// blockReason maps a blocked event's error to a small label-safe enum.
// safehttp surfaces three distinct sentinel errors; the catch-all keeps
// us future-proof if more get added.
func blockReason(ev safehttp.EgressEvent) string {
	if ev.Err == nil {
		return "unknown"
	}
	switch {
	case errors.Is(ev.Err, safehttp.ErrBlocked):
		return "ssrf"
	case errors.Is(ev.Err, safehttp.ErrInvalidScheme):
		return "scheme"
	case errors.Is(ev.Err, safehttp.ErrMissingHost):
		return "missing_host"
	case strings.Contains(ev.Err.Error(), "blocked port"):
		return "port"
	}
	return "other"
}
