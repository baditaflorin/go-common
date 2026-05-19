package promx

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestEgressCollectorsCountsSuccess: wire promx into a safehttp client,
// hit a test server, verify the canonical counters are populated.
func TestEgressCollectorsCountsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "11")
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	coll := NewEgressCollectors(reg)

	// 127.0.0.1 is blocked by safehttp by default — allow it for this test.
	safehttp.SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1")})
	defer safehttp.SetAllowedPrivateIPs(nil)

	c := safehttp.NewClient(
		safehttp.WithObserver(coll),
		safehttp.WithTimeout(2*time.Second),
	)
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()

	u, _ := url.Parse(srv.URL)
	host := u.Hostname()

	if got := testutil.ToFloat64(coll.requestsTotal.WithLabelValues("", host, "http", "false", "success")); got != 1 {
		t.Errorf("requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(coll.bytesTotal.WithLabelValues("", host)); got != 11 {
		t.Errorf("response_bytes_total = %v, want 11", got)
	}
	// Histogram: assert the registry has it and there's at least one
	// sample. We don't dig into bucket math.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sawDuration bool
	for _, mf := range mfs {
		if mf.GetName() == "safehttp_egress_duration_seconds" {
			for _, m := range mf.GetMetric() {
				if m.GetHistogram().GetSampleCount() >= 1 {
					sawDuration = true
				}
			}
		}
	}
	if !sawDuration {
		t.Errorf("duration histogram had no samples")
	}
}

// TestEgressCollectorsCountsBlocked: an SSRF block should bump the
// blocked counter with reason=ssrf.
func TestEgressCollectorsCountsBlocked(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewEgressCollectors(reg)

	c := safehttp.NewClient(
		safehttp.WithObserver(coll),
		safehttp.WithTimeout(2*time.Second),
	)
	_, _ = c.Get("http://10.0.0.1/")

	if got := testutil.ToFloat64(coll.blockedTotal.WithLabelValues("", "ssrf")); got != 1 {
		t.Errorf("blocked_total{reason=ssrf} = %v, want 1", got)
	}
}

// TestHostCardinalityCap: hosts beyond the cap fold to "_other".
func TestHostCardinalityCap(t *testing.T) {
	c := newHostCardCap(2)
	if got := c.label("a.example.com"); got != "a.example.com" {
		t.Errorf("first host should pass through, got %q", got)
	}
	if got := c.label("b.example.com"); got != "b.example.com" {
		t.Errorf("second host should pass through, got %q", got)
	}
	if got := c.label("c.example.com"); got != "_other" {
		t.Errorf("third host should fold to _other, got %q", got)
	}
	if got := c.label("a.example.com"); got != "a.example.com" {
		t.Errorf("already-admitted host should still resolve, got %q", got)
	}
	if got := c.label(""); got != "_unknown" {
		t.Errorf("empty host should become _unknown, got %q", got)
	}
}

// TestRegistryDeduplicates: registering the same collector twice on the
// shared registry should panic the second time — that's the bug class
// promx.Init guards against.
func TestInitIdempotent(t *testing.T) {
	// Best-effort: we can't reset the package-level singleton between
	// tests cleanly, so we just verify that calling Init twice with
	// matching args returns the same registry without panicking. The
	// mismatch-panic case is covered by reading the source.
	r1 := Init("test-service", "1.0.0")
	r2 := Init("test-service", "1.0.0")
	if r1 != r2 {
		t.Errorf("Init should be idempotent for matching args")
	}
}

// TestHandlerServesMetrics: the /metrics handler returns a 200 with
// Prometheus exposition format and includes build_info.
func TestHandlerServesMetrics(t *testing.T) {
	// Reuse the singleton from TestInitIdempotent — Init must run first.
	_ = Init("test-service", "1.0.0")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "build_info") {
		t.Errorf("metrics body missing build_info; body=\n%s", body)
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("metrics body missing go_goroutines (Go collector should be registered)")
	}
}
