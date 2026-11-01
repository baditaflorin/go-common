package promx

import (
	"fmt"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAuthCollectorsSPIFFEMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewAuthCollectors(reg)

	// No series until a service turns SPIFFE on.
	if n := testutil.CollectAndCount(c.spiffeFailures) + testutil.CollectAndCount(c.spiffeFallback); n != 0 {
		t.Fatalf("%d series before any SPIFFE event", n)
	}

	c.ObserveAuth(middleware.AuthEvent{Source: middleware.AuthSourceSPIFFEMTLS, Result: middleware.AuthResultAllow})
	c.ObserveAuth(middleware.AuthEvent{Source: middleware.AuthSourceSPIFFEJWT, Result: middleware.AuthResultAllow})
	c.ObserveIdentityFailure(middleware.AuthSourceSPIFFEJWT, "expired_token")
	c.ObserveIdentityFailure(middleware.AuthSourceSPIFFEJWT, "free text with a secret ak_123")
	c.ObserveIdentityFailure("whatever", "unmapped_id")
	c.ObserveIdentityFallback("agent-tools", "no_svid")
	c.ObserveIdentityFallback("agent-tools", "not-a-reason")

	want := `
# HELP spiffe_apikey_fallback_total Requests authenticated with an API key although the principal has a SPIFFE mapping (SPIFFE expected, not used), by reason (no_svid or the failure reason).
# TYPE spiffe_apikey_fallback_total counter
spiffe_apikey_fallback_total{principal="agent-tools",reason="no_svid",service="` + ServiceID() + `"} 1
spiffe_apikey_fallback_total{principal="agent-tools",reason="other",service="` + ServiceID() + `"} 1
`
	if err := testutil.CollectAndCompare(c.spiffeFallback, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"spiffe-mtls", "spiffe-jwt"} {
		if testutil.ToFloat64(c.authTotal.WithLabelValues(ServiceID(), src, "allow")) != 1 {
			t.Fatalf("apikey_auth_total{source=%q} not counted", src)
		}
	}
	if v := testutil.ToFloat64(c.spiffeFailures.WithLabelValues(ServiceID(), "spiffe-jwt", "other")); v != 1 {
		t.Fatalf("unknown reason must land in other, got %v", v)
	}
	if v := testutil.ToFloat64(c.spiffeFailures.WithLabelValues(ServiceID(), "other", "unmapped_id")); v != 1 {
		t.Fatalf("unknown source must land in other, got %v", v)
	}
}

func TestAuthCollectorsFallbackPrincipalsAreBounded(t *testing.T) {
	c := NewAuthCollectors(prometheus.NewRegistry())
	for i := 0; i < maxFallbackPrincipals+50; i++ {
		c.ObserveIdentityFallback(fmt.Sprintf("svc-%d", i), "no_svid")
	}
	if n := testutil.CollectAndCount(c.spiffeFallback); n != maxFallbackPrincipals+1 {
		t.Fatalf("%d series, want %d plus _other", n, maxFallbackPrincipals)
	}
}
