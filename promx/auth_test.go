package promx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// stubVerifier returns the configured result/err for every Verify.
type stubVerifier struct {
	res *apikey.VerifyResult
	err error
}

func (s *stubVerifier) Verify(_ context.Context, _ string) (*apikey.VerifyResult, error) {
	return s.res, s.err
}

// TestAuthCollectorsBypass: requests to /health hit the bypass path and
// produce one auth_total{source=bypass,result=allow}.
func TestAuthCollectorsBypass(t *testing.T) {
	reg := prometheus.NewRegistry()
	auth := NewAuthCollectors(reg)
	mw := middleware.TokenAuthKeystore(middleware.KeystoreOpts{
		Verifier: &stubVerifier{res: &apikey.VerifyResult{User: "x"}},
		Observer: auth,
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))

	if got := testutil.ToFloat64(auth.authTotal.WithLabelValues(auth.service, "bypass", "allow")); got != 1 {
		t.Errorf("auth_total{bypass,allow} = %v, want 1", got)
	}
}

// TestAuthCollectorsGatewayHeader: a request with X-Auth-User set is
// trusted by the middleware and produces auth_total{source=gateway}.
func TestAuthCollectorsGatewayHeader(t *testing.T) {
	reg := prometheus.NewRegistry()
	auth := NewAuthCollectors(reg)
	mw := middleware.TokenAuthKeystore(middleware.KeystoreOpts{
		Verifier: &stubVerifier{},
		Observer: auth,
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	r := httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set(header.AuthUser, "alice")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if got := testutil.ToFloat64(auth.authTotal.WithLabelValues(auth.service, "gateway", "allow")); got != 1 {
		t.Errorf("auth_total{gateway,allow} = %v, want 1", got)
	}
}

// TestAuthCollectorsKeystoreAllow: a request that falls through to the
// verifier on a successful Verify is recorded as source=keystore and
// also adds a keystore_call_duration sample.
func TestAuthCollectorsKeystoreAllow(t *testing.T) {
	reg := prometheus.NewRegistry()
	auth := NewAuthCollectors(reg)
	mw := middleware.TokenAuthKeystore(middleware.KeystoreOpts{
		Verifier: &stubVerifier{res: &apikey.VerifyResult{User: "alice"}},
		Observer: auth,
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	r := httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set("Authorization", "Bearer test-key")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if got := testutil.ToFloat64(auth.authTotal.WithLabelValues(auth.service, "keystore", "allow")); got != 1 {
		t.Errorf("auth_total{keystore,allow} = %v, want 1", got)
	}
	// Histogram should have one sample on the allow branch.
	mfs, _ := reg.Gather()
	var saw bool
	for _, mf := range mfs {
		if mf.GetName() == "apikey_keystore_call_duration_seconds" {
			for _, m := range mf.GetMetric() {
				if m.GetHistogram().GetSampleCount() >= 1 {
					saw = true
				}
			}
		}
	}
	if !saw {
		t.Errorf("keystore_call_duration_seconds had no sample")
	}
}

// TestCacheObserverFreshAndInner: exercise the Cache.Verify paths that
// matter — fresh hit (no upstream call) and inner_ok (upstream called).
func TestCacheObserverFreshAndInner(t *testing.T) {
	reg := prometheus.NewRegistry()
	auth := NewAuthCollectors(reg)

	c := apikey.NewCache(&stubVerifier{res: &apikey.VerifyResult{User: "x"}})
	c.Observer = auth

	// First call: upstream populated, inner_ok.
	if _, err := c.Verify(context.Background(), "k"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Second call: within FreshTTL → fresh.
	if _, err := c.Verify(context.Background(), "k"); err != nil {
		t.Fatalf("verify: %v", err)
	}

	if got := testutil.ToFloat64(auth.cacheTotal.WithLabelValues(auth.service, "inner_ok")); got != 1 {
		t.Errorf("cache_total{inner_ok} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(auth.cacheTotal.WithLabelValues(auth.service, "fresh")); got != 1 {
		t.Errorf("cache_total{fresh} = %v, want 1", got)
	}
}

// TestCacheObserverStale: with an expired-fresh entry and the upstream
// returning ErrKeystoreUnavailable, the cache serves the stale entry
// (under StaleTTL) and the observer records stale + stale_age.
func TestCacheObserverStale(t *testing.T) {
	reg := prometheus.NewRegistry()
	auth := NewAuthCollectors(reg)

	stub := &stubVerifier{res: &apikey.VerifyResult{User: "x"}}
	c := apikey.NewCache(stub)
	c.FreshTTL = 10 * time.Millisecond
	c.StaleTTL = 1 * time.Hour
	c.Observer = auth

	// Populate.
	if _, err := c.Verify(context.Background(), "k"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Wait past FreshTTL, then flip upstream to unavailable.
	time.Sleep(20 * time.Millisecond)
	stub.res = nil
	stub.err = errors.New("upstream boom: " + apikey.ErrKeystoreUnavailable.Error())
	// Wrap so errors.Is(err, ErrKeystoreUnavailable) is false but the
	// Cache treats anything-not-ErrInvalidKey as "transient" anyway.
	// To exercise the stale path we need an error that ISN'T
	// ErrInvalidKey — any wrapped error works.

	if _, err := c.Verify(context.Background(), "k"); err != nil {
		t.Fatalf("stale-path verify returned err: %v", err)
	}
	if got := testutil.ToFloat64(auth.cacheTotal.WithLabelValues(auth.service, "stale")); got != 1 {
		t.Errorf("cache_total{stale} = %v, want 1", got)
	}
}
