package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/header"
)

// TestWithKeystoreAuth_Wires confirms the option mounts a middleware
// without blowing up at construction time, even with no env vars set
// (apikey.New uses defaults; failures are deferred to first request).
func TestWithKeystoreAuth_Wires(t *testing.T) {
	cfg := &config.Config{AppName: "test", Version: "0.0.0", Port: "0"}
	srv := New(cfg, WithKeystoreAuth())
	if srv == nil {
		t.Fatal("server is nil")
	}
	// at least the three defaults plus one we just added
	if len(srv.Middlewares) < 4 {
		t.Fatalf("expected ≥4 middlewares (3 default + keystore auth), got %d",
			len(srv.Middlewares))
	}
}

// TestWithKeystoreAuthTier_Wires mirrors TestWithKeystoreAuth_Wires for the
// tier-gated variant — construction must not blow up regardless of
// requiredTier/enforce, since actual deny/allow behavior is covered by
// middleware's own TokenAuthKeystore tests.
func TestWithKeystoreAuthTier_Wires(t *testing.T) {
	cfg := &config.Config{AppName: "test", Version: "0.0.0", Port: "0"}
	srv := New(cfg, WithKeystoreAuthTier("vetted-pentest", true))
	if srv == nil {
		t.Fatal("server is nil")
	}
	if len(srv.Middlewares) < 4 {
		t.Fatalf("expected ≥4 middlewares (3 default + keystore auth), got %d",
			len(srv.Middlewares))
	}
}

func TestWithKeystoreAuthTierTrustedProxyNoCacheRechecksOneUse(t *testing.T) {
	var verifyCalls atomic.Int32
	keystore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/verify" || r.Method != http.MethodPost || r.Header.Get(header.VerifyKey) != "one-use-test-key" {
			t.Errorf("unexpected verify request: method=%s path=%s key=%q", r.Method, r.URL.Path, r.Header.Get(header.VerifyKey))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if verifyCalls.Add(1) != 1 {
			// Model the authoritative API-key service after the atomic claim:
			// a replay cannot be admitted merely because the first request was
			// valid.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set(header.AuthUser, "fleet-build-broker-pki-renewer")
		w.Header().Set(header.AuthScope, "infra-privileged")
		w.Header().Set(header.AuthTier, "infra-privileged")
		w.WriteHeader(http.StatusOK)
	}))
	defer keystore.Close()
	t.Setenv("APIKEY_SERVICE_URL", keystore.URL)

	cfg := &config.Config{AppName: "test", Version: "0.0.0", Port: "0"}
	srv := New(cfg, WithKeystoreAuthTierTrustedProxyNoCache("infra-privileged", true, []string{"10.10.10.10/32"}))
	srv.Mux.HandleFunc("/protected", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	protected := httptest.NewServer(srv.Handler())
	defer protected.Close()

	request := func() int {
		req, err := http.NewRequest(http.MethodPost, protected.URL+"/protected", nil)
		if err != nil {
			t.Fatalf("new protected request: %v", err)
		}
		req.Header.Set(header.APIKey, "one-use-test-key")
		resp, err := protected.Client().Do(req)
		if err != nil {
			t.Fatalf("call protected endpoint: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := request(); got != http.StatusNoContent {
		t.Fatalf("first one-use request: got %d, want %d", got, http.StatusNoContent)
	}
	if got := request(); got != http.StatusUnauthorized {
		t.Fatalf("replay: got %d, want %d", got, http.StatusUnauthorized)
	}
	if got := verifyCalls.Load(); got != 2 {
		t.Fatalf("direct requests must each call authoritative verify: got %d calls, want 2", got)
	}
}

func TestWithKeystoreAuthAcceptsCommonCrawlStageKeyOnlyInIsolatedStaging(t *testing.T) {
	var verifyCalls atomic.Int32
	keystore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer keystore.Close()
	t.Setenv("APIKEY_SERVICE_URL", keystore.URL)
	t.Setenv("COMMONCRAWL_STAGE_API_KEY", "stage-probe-key")

	for _, tc := range []struct {
		name        string
		environment string
		source      string
		wantStatus  int
		wantVerify  int32
	}{
		{name: "isolated archive staging", environment: "staging", source: "commoncrawl", wantStatus: http.StatusNoContent, wantVerify: 0},
		{name: "live staging", environment: "staging", source: "live", wantStatus: http.StatusUnauthorized, wantVerify: 1},
		{name: "production archive source", environment: "production", source: "commoncrawl", wantStatus: http.StatusUnauthorized, wantVerify: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLEET_ENVIRONMENT", tc.environment)
			t.Setenv(fleetfetch.EnvSource, tc.source)
			cfg := &config.Config{AppName: "test", Version: "0.0.0", Port: "0"}
			srv := New(cfg, WithKeystoreAuth())
			srv.Mux.HandleFunc("/protected", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			protected := httptest.NewServer(srv.Handler())
			defer protected.Close()

			req, err := http.NewRequest(http.MethodGet, protected.URL+"/protected", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(header.APIKey, "stage-probe-key")
			resp, err := protected.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status=%d want=%d", resp.StatusCode, tc.wantStatus)
			}
			if got := verifyCalls.Load(); got != tc.wantVerify {
				t.Fatalf("keystore verify calls=%d want=%d", got, tc.wantVerify)
			}
		})
	}
}

// TestWithKeystoreAuthPublicPrefixes_BypassesOnlyTheDeclaredPrefix exercises
// the option end to end against srv.Handler(): a declared prefix serves with
// no key, while everything else keeps requiring one — the shape
// go-pentest-oob-collector needs for its public GET /c/<token> catch route.
func TestWithKeystoreAuthPublicPrefixes_BypassesOnlyTheDeclaredPrefix(t *testing.T) {
	cfg := &config.Config{AppName: "test", Version: "0.0.0", Port: "0"}
	srv := New(cfg, WithKeystoreAuthPublicPrefixes([]string{"/c/"}, "default_token"))
	srv.Mux.HandleFunc("/c/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	srv.Mux.HandleFunc("/hits", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := srv.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/c/sometoken", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/c/sometoken with no key: want 200, got %d", rr.Code)
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/hits", nil))
	if rr2.Code == http.StatusOK {
		t.Fatal("/hits with no key must still be gated — the declared prefix must not widen the whole service")
	}
}
