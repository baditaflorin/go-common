package promx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
