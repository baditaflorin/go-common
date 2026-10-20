package promx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/baditaflorin/go-common/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
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
//
// The "method" label is the request method when it is one of the nine
// standard ones (GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS, CONNECT,
// TRACE) and "OTHER" for any other token. Go's server accepts any token as
// a method and this middleware runs before authentication, so without the
// fold anyone who can reach the port could mint a new series set per
// distinct method.
//
// The "status" label is the response status code when it is a registered one
// (about sixty: 200, 404, 429, 503...) and its class ("4xx", "5xx") when it is
// not, because a handler that relays an upstream's status writes whatever the
// upstream chose, anything from 100 to 999. A code outside 100-599 that nobody
// registered is "_other". The "route" label is a BoundedLabel: past the limit,
// and for a value that is not valid UTF-8 or longer than MaxLabelValueLen, it is
// "_other".
type HTTPCollectors struct {
	service string

	requestsTotal *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	responseSize  *prometheus.HistogramVec
	inFlight      *prometheus.GaugeVec

	// inFlightG is the inFlight gauge curried to the constant `service`
	// label, cached once so the per-request Inc/Dec skip a WithLabelValues
	// map lookup+lock each (it fires twice per request otherwise).
	inFlightG prometheus.Gauge

	routeFn func(*http.Request) string

	// routeCap bounds the cardinality of the "route" label. Routes beyond
	// the cap fold to "_other" so a service with parameterised paths or a
	// scanner spraying distinct URLs can't blow up Prometheus series count
	// (and the scrape server's memory). The same BoundedLabel the
	// egress/backoff/fleetfetch collectors use for "host". See WithRouteLimit.
	routeCap *BoundedLabel
}

// HTTPOption configures NewHTTPCollectors.
type HTTPOption func(*httpOpts)

type httpOpts struct {
	durationBuckets []float64
	sizeBuckets     []float64
	routeFn         func(*http.Request) string
	routeLimit      int
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

// WithRouteLimit caps how many distinct "route" label values the inbound
// HTTP collectors will emit before folding the rest into "_other". This is
// the safety net that keeps an unbounded route label (the default raw
// r.URL.Path, parameterised paths, or scanner traffic) from exploding the
// Prometheus series count and the scrape server's memory. Default: 512.
// A value <= 0 restores the default. Raise it for services with a large
// but bounded templated-route set; it does not need raising when a proper
// WithRouteFunc keeps cardinality naturally low.
func WithRouteLimit(n int) HTTPOption {
	return func(o *httpOpts) { o.routeLimit = n }
}

// methodOther is the "method" label for any request method outside the
// standard set. See methodLabel.
const methodOther = "OTHER"

// methodLabel returns m when it is a standard HTTP method and methodOther
// otherwise, so the "method" label has at most ten values whatever a client
// sends. It returns m itself, not a copy, so it does not allocate.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return m
	}
	return methodOther
}

// statusLabels is the "status" label for every code net/http lets a handler write
// (it panics outside 100-999): the code itself when it is a registered one
// (http.StatusText knows about sixty), else its class ("499" is "4xx", "520" is
// "5xx", which a "5.." matcher still selects), else OtherLabel. A table makes it
// one load and no allocation on the request path; strconv.Itoa allocated for every
// three-digit code.
var statusLabels = func() (t [1000]string) {
	for code := 100; code < len(t); code++ {
		switch {
		case http.StatusText(code) != "":
			t[code] = strconv.Itoa(code)
		case code < 600:
			t[code] = string([]byte{byte('0' + code/100), 'x', 'x'})
		default:
			t[code] = OtherLabel
		}
	}
	return t
}()

// statusLabel returns the "status" label for a response status code. A handler
// can write any code from 100 to 999, and one that relays an upstream's status
// writes whatever the upstream chose, so the label folds the codes nobody
// registered into their class instead of giving each its own series set.
func statusLabel(code int) string {
	if code < 0 || code >= len(statusLabels) || statusLabels[code] == "" {
		return OtherLabel
	}
	return statusLabels[code]
}

// NewHTTPCollectors registers the canonical inbound HTTP collectors on
// reg and returns the wrapper. reg may be nil — the shared
// promx.Registry() is used in that case.
func NewHTTPCollectors(reg prometheus.Registerer, opts ...HTTPOption) *HTTPCollectors {
	o := &httpOpts{
		durationBuckets: prometheus.DefBuckets,
		sizeBuckets:     []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216},
		routeFn:         func(r *http.Request) string { return r.URL.Path },
		routeLimit:      512,
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.routeLimit <= 0 {
		o.routeLimit = 512
	}
	if reg == nil {
		reg = Registry()
	}
	service := ServiceID()

	c := &HTTPCollectors{
		service:  service,
		routeFn:  o.routeFn,
		routeCap: NewBoundedLabel(o.routeLimit),
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
	c.inFlightG = c.inFlight.WithLabelValues(service)
	reg.MustRegister(c.requestsTotal, c.duration, c.responseSize, c.inFlight)
	return c
}

// WithRoute returns a view of c that labels "route" with fn and keeps at most
// limit distinct route values, recording into the same registered collectors as
// c. It is how a caller that did not build the collectors (server.New, through
// AutoWire, builds them with the defaults) changes the route label: c itself is
// not modified, so the process-wide set AutoWire hands out stays as it was for
// every other caller, and nothing is registered twice.
//
// A nil fn keeps c's route function. A limit <= 0 keeps c's limit. The view has
// its own admitted-route set, so it starts empty. Series already recorded
// through c stay in the registry: a view changes what is recorded from now on.
func (c *HTTPCollectors) WithRoute(fn func(*http.Request) string, limit int) *HTTPCollectors {
	v := *c
	if fn != nil {
		v.routeFn = fn
	}
	if limit <= 0 {
		limit = c.routeCap.limit
	}
	v.routeCap = NewBoundedLabel(limit)
	return &v
}

// Middleware returns the net/http middleware that records every request
// on the collectors. Use middleware.Chain to compose it with logging,
// auth, etc. The /metrics endpoint should NOT be wrapped — it would
// recurse into the collectors it's exposing.
func (c *HTTPCollectors) Middleware() middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route := c.routeCap.Value(c.routeFn(r))
			method := methodLabel(r.Method)
			c.inFlightG.Inc()
			defer c.inFlightG.Dec()

			rw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rw, r)
			dur := time.Since(start)

			status := statusLabel(rw.status)
			c.requestsTotal.WithLabelValues(c.service, method, route, status).Inc()
			durationObserver := c.duration.WithLabelValues(c.service, method, route)
			if spanContext := trace.SpanContextFromContext(r.Context()); spanContext.IsSampled() {
				if exemplarObserver, ok := durationObserver.(prometheus.ExemplarObserver); ok {
					exemplarObserver.ObserveWithExemplar(dur.Seconds(), prometheus.Labels{"trace_id": spanContext.TraceID().String()})
				} else {
					durationObserver.Observe(dur.Seconds())
				}
			} else {
				durationObserver.Observe(dur.Seconds())
			}
			if rw.bytes > 0 {
				c.responseSize.WithLabelValues(c.service, method, route).Observe(float64(rw.bytes))
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

// Flush + Unwrap let streaming handlers (SSE, tail -f, long-poll) work through
// this wrapper — it's the innermost writer the handler actually holds, so
// without Flush here w.(http.Flusher) fails and streaming bails with
// "streaming unsupported".
func (w *countingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
