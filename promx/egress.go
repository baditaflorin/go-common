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
//	safehttp_egress_duration_seconds{service, host, via_proxy}
//	safehttp_egress_response_bytes_total{service, host}
//	safehttp_egress_blocked_total{service, reason}
//
// "host" cardinality is capped — see HostLimit option. Hosts beyond the
// cap are folded into the literal label "_other" so a runaway scanner
// cannot blow up Prometheus storage. "outcome" is a fixed-size enum drawn
// from safehttp.EgressOutcome (~9 values), so the product label space is
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
	hostLimit int
	buckets   []float64
}

// WithHostLimit caps the number of distinct host label values that will be
// emitted. Hosts beyond the cap get folded into "_other". Default: 256 —
// big enough for normal fleet egress, small enough to keep TSDB sane if a
// service starts scanning the internet.
func WithHostLimit(n int) EgressOption {
	return func(o *egressOpts) { o.hostLimit = n }
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
	o := &egressOpts{hostLimit: 256, buckets: prometheus.DefBuckets}
	for _, opt := range opts {
		opt(o)
	}
	if reg == nil {
		reg = Registry()
	}
	service := ServiceID()

	c := &EgressCollectors{
		service: service,
		hosts:   newHostCardCap(o.hostLimit),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "safehttp_egress_requests_total",
			Help: "Total outbound HTTP requests made via safehttp clients, labelled by destination host, scheme, proxy usage, and outcome bucket.",
		}, []string{"service", "host", "scheme", "via_proxy", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "safehttp_egress_duration_seconds",
			Help:    "Outbound HTTP request duration in seconds (full round-trip including TLS + body fetch).",
			Buckets: o.buckets,
		}, []string{"service", "host", "via_proxy"}),
		bytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "safehttp_egress_response_bytes_total",
			Help: "Total response Content-Length bytes received per outbound host (0 if length unknown).",
		}, []string{"service", "host"}),
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
	c.duration.WithLabelValues(c.service, host, viaProxy).Observe(ev.Duration.Seconds())
	if ev.Bytes > 0 {
		c.bytesTotal.WithLabelValues(c.service, host).Add(float64(ev.Bytes))
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
