package promx

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/proxysupplier"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAutoWireExposesWebshareDirectCollectorIdempotently(t *testing.T) {
	AutoWire("go-autowire-service", "test")
	first := AutoWebshareDirect()
	if first == nil {
		t.Fatal("AutoWebshareDirect() = nil after AutoWire")
	}
	first.snapshot = func() proxysupplier.WebshareDirectPoolState {
		return proxysupplier.WebshareDirectPoolState{Total: 7, InCooldown: 2, Eligible: 5}
	}

	// Re-entering AutoWire for the same process identity must reuse the
	// collector rather than attempting a duplicate registration.
	AutoWire("go-autowire-service", "test")
	if got := AutoWebshareDirect(); got != first {
		t.Fatal("same-registry AutoWire replaced the webshare_direct collector")
	}

	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("metrics status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, sample := range []string{
		`proxysupplier_webshare_direct_pool_size{service="go-autowire-service"} 7`,
		`proxysupplier_webshare_direct_pool_in_cooldown{service="go-autowire-service"} 2`,
		`proxysupplier_webshare_direct_pool_eligible{service="go-autowire-service"} 5`,
	} {
		if !strings.Contains(body, sample) {
			t.Errorf("/metrics missing %q", sample)
		}
	}

	// A new identity gets a new registry and collector, matching AutoWire's
	// existing test/rebind contract for every other collector family.
	AutoWire("go-autowire-rebound", "test")
	if got := AutoWebshareDirect(); got == nil || got == first {
		t.Fatal("registry rebind did not create a fresh webshare_direct collector")
	}
}

func TestWebshareDirectCollectorsExposeConsistentPoolSnapshot(t *testing.T) {
	Init("go-test-service", "test")
	reg := prometheus.NewRegistry()
	c := NewWebshareDirectCollectors(reg)
	c.snapshot = func() proxysupplier.WebshareDirectPoolState {
		return proxysupplier.WebshareDirectPoolState{
			Total:      5,
			InCooldown: 2,
			Eligible:   3,
		}
	}

	want := `
# HELP proxysupplier_webshare_direct_pool_eligible Current proxy IPs eligible for selection by the webshare_direct supplier.
# TYPE proxysupplier_webshare_direct_pool_eligible gauge
proxysupplier_webshare_direct_pool_eligible{service="go-test-service"} 3
# HELP proxysupplier_webshare_direct_pool_in_cooldown Current proxy IPs excluded by the webshare_direct supplier because their cooldown has not elapsed.
# TYPE proxysupplier_webshare_direct_pool_in_cooldown gauge
proxysupplier_webshare_direct_pool_in_cooldown{service="go-test-service"} 2
# HELP proxysupplier_webshare_direct_pool_size Total proxy IPs loaded by the webshare_direct supplier from its last successful catalog refresh.
# TYPE proxysupplier_webshare_direct_pool_size gauge
proxysupplier_webshare_direct_pool_size{service="go-test-service"} 5
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"proxysupplier_webshare_direct_pool_size",
		"proxysupplier_webshare_direct_pool_in_cooldown",
		"proxysupplier_webshare_direct_pool_eligible",
	); err != nil {
		t.Fatal(err)
	}
}

func TestWebshareDirectCollectorsExposeZeroWhenDisabled(t *testing.T) {
	Init("go-disabled-service", "test")
	reg := prometheus.NewRegistry()
	c := NewWebshareDirectCollectors(reg)
	c.snapshot = func() proxysupplier.WebshareDirectPoolState {
		return proxysupplier.WebshareDirectPoolState{}
	}

	want := `
# HELP proxysupplier_webshare_direct_pool_eligible Current proxy IPs eligible for selection by the webshare_direct supplier.
# TYPE proxysupplier_webshare_direct_pool_eligible gauge
proxysupplier_webshare_direct_pool_eligible{service="go-disabled-service"} 0
# HELP proxysupplier_webshare_direct_pool_in_cooldown Current proxy IPs excluded by the webshare_direct supplier because their cooldown has not elapsed.
# TYPE proxysupplier_webshare_direct_pool_in_cooldown gauge
proxysupplier_webshare_direct_pool_in_cooldown{service="go-disabled-service"} 0
# HELP proxysupplier_webshare_direct_pool_size Total proxy IPs loaded by the webshare_direct supplier from its last successful catalog refresh.
# TYPE proxysupplier_webshare_direct_pool_size gauge
proxysupplier_webshare_direct_pool_size{service="go-disabled-service"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"proxysupplier_webshare_direct_pool_size",
		"proxysupplier_webshare_direct_pool_in_cooldown",
		"proxysupplier_webshare_direct_pool_eligible",
	); err != nil {
		t.Fatal(err)
	}
}

func TestWebshareDirectCollectorsExposeRefreshHealth(t *testing.T) {
	Init("go-test-service", "test")
	reg := prometheus.NewRegistry()
	c := NewWebshareDirectCollectors(reg)
	c.snapshot = func() proxysupplier.WebshareDirectPoolState {
		return proxysupplier.WebshareDirectPoolState{
			RefreshSuccesses: 4, RefreshFailures: 3, ConsecutiveRefreshFailures: 2, LastRefreshAgeSeconds: -1,
		}
	}
	want := `
# HELP proxysupplier_webshare_direct_last_refresh_age_seconds Seconds since the webshare_direct catalog was last refreshed successfully, -1 if it never was.
# TYPE proxysupplier_webshare_direct_last_refresh_age_seconds gauge
proxysupplier_webshare_direct_last_refresh_age_seconds{service="go-test-service"} -1
# HELP proxysupplier_webshare_direct_refresh_consecutive_failures Catalog refreshes that failed in a row since the last success (0 = healthy).
# TYPE proxysupplier_webshare_direct_refresh_consecutive_failures gauge
proxysupplier_webshare_direct_refresh_consecutive_failures{service="go-test-service"} 2
# HELP proxysupplier_webshare_direct_refresh_total Catalog refreshes of the webshare_direct supplier by result. A failed refresh is retried with backoff.
# TYPE proxysupplier_webshare_direct_refresh_total counter
proxysupplier_webshare_direct_refresh_total{result="error",service="go-test-service"} 3
proxysupplier_webshare_direct_refresh_total{result="ok",service="go-test-service"} 4
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"proxysupplier_webshare_direct_last_refresh_age_seconds",
		"proxysupplier_webshare_direct_refresh_consecutive_failures",
		"proxysupplier_webshare_direct_refresh_total",
	); err != nil {
		t.Fatal(err)
	}
}
