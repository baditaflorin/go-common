package promx

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/trace"
)

// TestHTTPMiddlewareRecordsRequest: a basic GET returning 200 increments
// requests_total, observes duration + response size, and resets in_flight
// to zero on completion.
func TestHTTPMiddlewareRecordsRequest(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewHTTPCollectors(reg)

	handler := coll.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/things/42", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, "GET", "/things/42", "200")); got != 1 {
		t.Errorf("requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(coll.inFlight.WithLabelValues(coll.service)); got != 0 {
		t.Errorf("in_flight = %v after handler returned, want 0", got)
	}
}

// TestHTTPMiddlewareRouteFunc: with a custom RouteFunc, the templated
// route (not the raw path) appears as the label — kills the
// per-distinct-id cardinality explosion.
func TestHTTPMiddlewareRouteFunc(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewHTTPCollectors(reg, WithRouteFunc(func(r *http.Request) string {
		return "/things/{id}"
	}))
	handler := coll.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, p := range []string{"/things/1", "/things/2", "/things/3"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}

	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, "GET", "/things/{id}", "200")); got != 3 {
		t.Errorf("requests_total for templated route = %v, want 3", got)
	}
}

// TestHTTPMiddlewareCapturesStatus: a non-200 response is reflected in
// the status label.
func TestHTTPMiddlewareCapturesStatus(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewHTTPCollectors(reg)
	handler := coll.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/x", nil))

	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, "POST", "/x", "418")); got != 1 {
		t.Errorf("requests_total{418} = %v, want 1", got)
	}
}

// TestHTTPRouteCardinalityCap: with a low route limit, distinct raw paths
// beyond the cap fold into "_other" so an unbounded route label cannot
// explode the Prometheus series count. The first N distinct routes keep
// their own series; everything after collapses.
func TestHTTPRouteCardinalityCap(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewHTTPCollectors(reg, WithRouteLimit(3))

	handler := coll.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	// 10 distinct paths; only 3 should keep their own route label.
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/u/"+strconv.Itoa(i), nil)
		handler.ServeHTTP(rec, req)
	}

	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, "GET", "/u/0", "200")); got != 1 {
		t.Errorf("first admitted route /u/0 = %v, want 1", got)
	}
	// 10 requests, 3 admitted distinct routes (/u/0../u/2) → 7 fold to _other.
	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, "GET", "_other", "200")); got != 7 {
		t.Errorf("_other route count = %v, want 7", got)
	}
	// A beyond-cap path must NOT have its own series.
	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues(coll.service, "GET", "/u/9", "200")); got != 0 {
		t.Errorf("beyond-cap route /u/9 = %v, want 0 (should have folded to _other)", got)
	}
}

// TestHTTPRouteLimitDefaultAndOverride: WithRouteLimit(0) restores the
// default; the cap field is always non-nil.
func TestHTTPRouteLimitDefaultAndOverride(t *testing.T) {
	for _, n := range []int{0, -5} {
		coll := NewHTTPCollectors(prometheus.NewRegistry(), WithRouteLimit(n))
		if coll.routeCap == nil {
			t.Fatalf("routeCap nil for WithRouteLimit(%d)", n)
		}
		if coll.routeCap.limit != 512 {
			t.Errorf("WithRouteLimit(%d) limit = %d, want default 512", n, coll.routeCap.limit)
		}
	}
}

// TestHTTPWithRouteIsAView: WithRoute changes what a view records without
// touching the collectors it came from, and both record into the same series.
func TestHTTPWithRouteIsAView(t *testing.T) {
	reg := prometheus.NewRegistry()
	base := NewHTTPCollectors(reg)
	view := base.WithRoute(func(*http.Request) string { return "/t/{id}" }, 2)
	serve := func(c *HTTPCollectors, path string) {
		c.Middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
			ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}

	serve(view, "/t/1")
	serve(view, "/t/2")
	serve(base, "/t/3")

	count := func(route string) float64 {
		return testutil.ToFloat64(base.requestsTotal.WithLabelValues(base.service, "GET", route, "200"))
	}
	if got := count("/t/{id}"); got != 2 {
		t.Errorf(`view route="/t/{id}" = %v, want 2`, got)
	}
	if got := count("/t/3"); got != 1 {
		t.Errorf("the original collectors changed: route=/t/3 = %v, want 1 (raw path)", got)
	}

	keep := base.WithRoute(nil, 0)
	if keep.routeFn == nil || keep.routeCap == base.routeCap || keep.routeCap.limit != base.routeCap.limit {
		t.Errorf("WithRoute(nil, 0) must keep the route function and limit, with its own admitted set: %+v", keep)
	}
	if view.routeCap.limit != 2 {
		t.Errorf("view limit = %d, want 2", view.routeCap.limit)
	}
}

// TestMethodLabel: the nine standard methods keep their own label; any other
// token, including a standard one in another case, is "OTHER".
func TestMethodLabel(t *testing.T) {
	for _, m := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace,
	} {
		if got := methodLabel(m); got != m {
			t.Errorf("methodLabel(%q) = %q, want %q", m, got, m)
		}
	}
	for _, m := range []string{"", "get", "Post", "PROPFIND", "PURGE", "BREW", "X1", "GET ", "G\u00c9T"} {
		if got := methodLabel(m); got != "OTHER" {
			t.Errorf("methodLabel(%q) = %q, want OTHER", m, got)
		}
	}
}

// TestHTTPMethodLabelIsBounded drives a loopback server with 300 distinct
// request methods, as anyone who can reach the port can: Go's server accepts any
// token as a method and the middleware runs before authentication. The scrape
// must stay small: one "OTHER" series set, not one per method.
func TestHTTPMethodLabelIsBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewHTTPCollectors(reg)
	ts := httptest.NewServer(coll.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})))
	defer ts.Close()

	send := func(method string) {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+"/probe", nil)
		if err != nil {
			t.Fatalf("NewRequest(%q): %v", method, err)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		_ = resp.Body.Close()
	}
	const odd = 300
	for i := 0; i < odd; i++ {
		send("X" + strconv.Itoa(i))
	}
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete, "get"} {
		send(m)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	perFamily := map[string]map[string]float64{}
	for _, family := range families {
		switch family.GetName() {
		case "http_requests_total", "http_request_duration_seconds", "http_response_size_bytes":
		default:
			continue
		}
		methods := map[string]float64{}
		for _, m := range family.GetMetric() {
			var n float64
			switch {
			case m.GetCounter() != nil:
				n = m.GetCounter().GetValue()
			case m.GetHistogram() != nil:
				n = float64(m.GetHistogram().GetSampleCount())
			}
			for _, l := range m.GetLabel() {
				if l.GetName() == "method" {
					methods[l.GetValue()] += n
				}
			}
		}
		perFamily[family.GetName()] = methods
	}
	for _, name := range []string{"http_requests_total", "http_request_duration_seconds", "http_response_size_bytes"} {
		methods, ok := perFamily[name]
		if !ok {
			t.Fatalf("%s: family missing from the scrape", name)
		}
		if len(methods) != 4 {
			t.Errorf("%s: %d distinct method labels, want 4 (GET, POST, DELETE, OTHER)", name, len(methods))
		}
		// 300 distinct tokens and the lower-case "get" all fold into OTHER.
		if methods["OTHER"] != odd+1 {
			t.Errorf("%s: OTHER = %v, want %d", name, methods["OTHER"], odd+1)
		}
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			if methods[m] != 1 {
				t.Errorf("%s: %s = %v, want 1", name, m, methods[m])
			}
		}
	}
}

func TestHTTPMiddlewareAttachesTraceExemplar(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewHTTPCollectors(reg)
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(t.Context(), spanContext)
	handler := coll.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/traceable", nil).WithContext(ctx)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "http_request_duration_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, bucket := range metric.GetHistogram().GetBucket() {
				exemplar := bucket.GetExemplar()
				if exemplar == nil {
					continue
				}
				for _, label := range exemplar.GetLabel() {
					if label.GetName() == "trace_id" && label.GetValue() == traceID.String() {
						return
					}
				}
			}
		}
	}
	t.Fatal("HTTP duration metric did not contain the sampled trace ID exemplar")
}
