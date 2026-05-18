package promx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/baditaflorin/go-common/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

// HTTPCollectors records inbound HTTP server traffic. Replaces the
// hand-rolled http_requests_total / http_request_duration_seconds /
// http_requests_in_flight collectors that ~30 fleet services have
// re-implemented today.
//
// Metrics exposed (labels in order):
//
//	http_requests_total{service, method, route, status}
//	http_request_duration_seconds{service, method, route}
//	http_response_size_bytes{service, method, route}
//	http_requests_in_flight{service}
//
// The "route" label uses a caller-supplied RouteFunc so a router that
// knows the templated path ("/users/{id}") can supply it instead of the
// raw path ("/users/42") — this is the single biggest fix vs. existing
// in-app metrics, where every distinct ID blows up label cardinality.
// Default RouteFunc returns r.URL.Path which preserves current behaviour
// for callers who haven't wired their router up yet.
type HTTPCollectors struct {
	service string

	requestsTotal *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	responseSize  *prometheus.HistogramVec
	inFlight      *prometheus.GaugeVec

	routeFn func(*http.Request) string
}

// HTTPOption configures NewHTTPCollectors.
type HTTPOption func(*httpOpts)

type httpOpts struct {
	durationBuckets []float64
	sizeBuckets     []float64
	routeFn         func(*http.Request) string
}

// WithHTTPDurationBuckets overrides the request-duration histogram
// buckets. Default: prometheus.DefBuckets (5ms…10s) — the same set every
// fleet service is already using.
func WithHTTPDurationBuckets(b []float64) HTTPOption {
	return func(o *httpOpts) { o.durationBuckets = b }
}

// WithHTTPSizeBuckets overrides the response-size histogram buckets.
// Default: 256B, 1KB, 4KB, 16KB, 64KB, 256KB, 1MB, 4MB, 16MB.
func WithHTTPSizeBuckets(b []float64) HTTPOption {
	return func(o *httpOpts) { o.sizeBuckets = b }
}

// WithRouteFunc supplies a function that returns the templated route for
// a request. Routers expose this differently:
//
//	chi:     chi.RouteContext(r.Context()).RoutePattern()
//	gin:     c.FullPath()        (wrap appropriately if using net/http chain)
//	std mux: pattern from http.ServeMux (Go 1.22+: mux.Handler(r).pattern)
//
// If nil (default), the middleware uses r.URL.Path — fine for small
// services, dangerous once you have parameterised routes.
func WithRouteFunc(fn func(*http.Request) string) HTTPOption {
	return func(o *httpOpts) { o.routeFn = fn }
}

// NewHTTPCollectors registers the canonical inbound HTTP collectors on
// reg and returns the wrapper. reg may be nil — the shared
// promx.Registry() is used in that case.
func NewHTTPCollectors(reg prometheus.Registerer, opts ...HTTPOption) *HTTPCollectors {
	o := &httpOpts{
		durationBuckets: prometheus.DefBuckets,
		sizeBuckets:     []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216},
		routeFn:         func(r *http.Request) string { return r.URL.Path },
	}
	for _, opt := range opts {
		opt(o)
	}
	if reg == nil {
		reg = Registry()
	}
	service := ServiceID()

	c := &HTTPCollectors{
		service: service,
		routeFn: o.routeFn,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total inbound HTTP requests handled, labelled by method, templated route, and response status.",
		}, []string{"service", "method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Inbound HTTP request duration in seconds (handler wall-clock).",
			Buckets: o.durationBuckets,
		}, []string{"service", "method", "route"}),
		responseSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_response_size_bytes",
			Help:    "Inbound HTTP response body size in bytes.",
			Buckets: o.sizeBuckets,
		}, []string{"service", "method", "route"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Number of HTTP requests currently being processed.",
		}, []string{"service"}),
	}
	reg.MustRegister(c.requestsTotal, c.duration, c.responseSize, c.inFlight)
	return c
}

// Middleware returns the net/http middleware that records every request
// on the collectors. Use middleware.Chain to compose it with logging,
// auth, etc. The /metrics endpoint should NOT be wrapped — it would
// recurse into the collectors it's exposing.
func (c *HTTPCollectors) Middleware() middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route := c.routeFn(r)
			c.inFlight.WithLabelValues(c.service).Inc()
			defer c.inFlight.WithLabelValues(c.service).Dec()

			rw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rw, r)
			dur := time.Since(start)

			status := strconv.Itoa(rw.status)
			c.requestsTotal.WithLabelValues(c.service, r.Method, route, status).Inc()
			c.duration.WithLabelValues(c.service, r.Method, route).Observe(dur.Seconds())
			if rw.bytes > 0 {
				c.responseSize.WithLabelValues(c.service, r.Method, route).Observe(float64(rw.bytes))
			}
		})
	}
}

// countingWriter is a local response-writer wrapper. We don't share
// middleware.wrappedWriter because that's an internal type — duplicating
// the ~10 lines here keeps promx decoupled from middleware internals.
type countingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *countingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
