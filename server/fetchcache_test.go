package server_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/safehttp"
	"github.com/baditaflorin/go-common/server"
)

// When FLEET_FETCH_CACHE_URL is set, server.New installs a process-wide
// safehttp fetch delegate so every subsequently-constructed safehttp
// client transparently routes its GETs through the fleet fetch cache.
func TestNew_WiresFetchCacheDelegate(t *testing.T) {
	// Ensure a clean baseline regardless of test ordering.
	safehttp.SetDefaultFetchDelegate(nil)
	t.Cleanup(func() { safehttp.SetDefaultFetchDelegate(nil) })

	t.Setenv(fleetfetch.EnvCacheURL, "http://go_infrastructure_fetch_cache:18205")

	cfg := &config.Config{AppName: "go_fetchcache_wiring_test", Version: "0.0.0", Port: "0"}
	_ = server.New(cfg)

	if safehttp.DefaultFetchDelegate() == nil {
		t.Fatalf("expected a default fetch delegate to be installed when %s is set", fleetfetch.EnvCacheURL)
	}
}

// Without the env var, server.New must NOT install a delegate (no
// silent fleet-wide egress rerouting on services that didn't opt in).
func TestNew_NoFetchCacheDelegateWithoutEnv(t *testing.T) {
	safehttp.SetDefaultFetchDelegate(nil)
	t.Cleanup(func() { safehttp.SetDefaultFetchDelegate(nil) })

	// t.Setenv with empty value guarantees the var is unset for this test
	// and restored afterward.
	t.Setenv(fleetfetch.EnvCacheURL, "")

	cfg := &config.Config{AppName: "go_fetchcache_noenv_test", Version: "0.0.0", Port: "0"}
	_ = server.New(cfg)

	if safehttp.DefaultFetchDelegate() != nil {
		t.Fatalf("did not expect a fetch delegate when %s is unset", fleetfetch.EnvCacheURL)
	}
}

// Regression for the 2026-08 fetch-cache request storm: the delegate's
// fleetfetch client received a bare cache 403, then its "direct" safehttp
// fallback resolved this same process-wide delegate at RoundTrip time. The
// request recursively re-entered the cache until its deadline. A cache auth
// failure must instead make the outer safehttp request fall through once to
// its direct transport, and the fleetfetch client's auth circuit must keep
// later requests from presenting the same bad credential again.
func TestDefaultFetchDelegate_Cache403FallsThroughDirectAndOpensCircuit(t *testing.T) {
	safehttp.SetDefaultFetchDelegate(nil)
	t.Cleanup(func() { safehttp.SetDefaultFetchDelegate(nil) })
	safehttp.SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1")})
	t.Cleanup(func() { safehttp.SetAllowedPrivateIPs(nil) })

	cacheHits := 0
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cacheHits++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer cache.Close()

	originHits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits++
		_, _ = io.WriteString(w, "direct origin")
	}))
	defer origin.Close()

	// Give cache and origin distinct hostnames. targetIsCacheHost correctly
	// ignores a request aimed at the configured cache host; httptest otherwise
	// gives both servers 127.0.0.1 with only the port differing.
	cacheURL := strings.Replace(cache.URL, "127.0.0.1", "localhost", 1)
	t.Setenv(fleetfetch.EnvCacheURL, cacheURL)
	t.Setenv(fleetfetch.EnvAPIKey, "rejected-test-key")
	_ = server.New(&config.Config{AppName: "go_fetchcache_auth_circuit_test", Version: "0.0.0", Port: "0"})

	client := safehttp.NewClient(safehttp.WithTimeout(2 * time.Second))
	for call := 0; call < 2; call++ {
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatalf("call %d: %v", call+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "direct origin" {
			t.Fatalf("call %d: status=%d body=%q", call+1, resp.StatusCode, body)
		}
	}
	if cacheHits != 1 {
		t.Fatalf("auth circuit did not suppress repeated cache auth: hits=%d, want 1", cacheHits)
	}
	if originHits != 2 {
		t.Fatalf("outer safehttp did not fall through direct: origin hits=%d, want 2", originHits)
	}
}

// Common Crawl errors must stop at the archive boundary. A delegate error
// normally tells safehttp to fetch the live origin, which is correct for
// source=live but violates the archive-only contract.
func TestDefaultFetchDelegate_CommonCrawlFailureDoesNotFetchOrigin(t *testing.T) {
	safehttp.SetDefaultFetchDelegate(nil)
	t.Cleanup(func() { safehttp.SetDefaultFetchDelegate(nil) })
	safehttp.SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1")})
	t.Cleanup(func() { safehttp.SetAllowedPrivateIPs(nil) })

	cacheHits := 0
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cacheHits++
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(cache.Close)
	// The skip-self guard compares hostnames, so keep this cache hostname
	// distinct from the 127.0.0.1 origin while still resolving locally.
	cacheURL := strings.Replace(cache.URL, "127.0.0.1", "localhost", 1)
	t.Setenv(fleetfetch.EnvCacheURL, cacheURL)
	t.Setenv(fleetfetch.EnvAPIKey, "rejected-test-key")
	t.Setenv(fleetfetch.EnvSource, "live")

	originHits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits++
		_, _ = io.WriteString(w, "live origin")
	}))
	t.Cleanup(origin.Close)

	srv := server.New(&config.Config{AppName: "go_fetchcache_commoncrawl_fail_closed_test", Version: "0.0.0", Port: "0"})
	srv.Mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, r.URL.Query().Get("target"), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// This service's target-page client deliberately bypasses the default
		// cache for live probes. An explicit Common Crawl request must still
		// use the archive, while source=live must retain the direct behavior.
		resp, err := safehttp.NewClient(safehttp.WithTimeout(2*time.Second), safehttp.WithoutFetchCache()).Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	handler := srv.Handler()
	request := func(source string) *httptest.ResponseRecorder {
		q := url.Values{"target": []string{origin.URL}, "source": []string{source}}
		req := httptest.NewRequest(http.MethodGet, "/probe?"+q.Encode(), nil)
		if source == "commoncrawl" {
			// A caller-controlled copy of the internal marker must not turn an
			// archive-only request into a direct live fetch.
			req.Header.Set("X-Fetch-Cache-Hop", "1")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	archive := request("commoncrawl")
	if archive.Code != http.StatusBadGateway || archive.Body.String() != "Common Crawl archive fetch unavailable" {
		t.Fatalf("archive response = %d %q, want synthetic 502", archive.Code, archive.Body.String())
	}
	if originHits != 0 {
		t.Fatalf("Common Crawl failure reached live origin %d times", originHits)
	}
	if cacheHits != 1 {
		t.Fatalf("cache hits = %d, want one archive attempt", cacheHits)
	}

	// A live request remains allowed to degrade to the origin when cache
	// authentication fails; the safehttp delegate's fail-open behavior stays
	// intact for the existing production path.
	live := request("live")
	if live.Code != http.StatusOK || live.Body.String() != "live origin" {
		t.Fatalf("live response = %d %q, want origin 200", live.Code, live.Body.String())
	}
	if originHits != 1 {
		t.Fatalf("live fallback origin hits = %d, want one", originHits)
	}
}

// The delegate is a transparent cache in front of safehttp: a body the fleetfetch
// client had to cut at its limit must not reach the caller as if it were whole. The
// request falls through to the direct path instead, and the caller gets every byte.
func TestDefaultFetchDelegate_BodyOverTheLimitFallsThroughDirect(t *testing.T) {
	safehttp.SetDefaultFetchDelegate(nil)
	t.Cleanup(func() { safehttp.SetDefaultFetchDelegate(nil) })
	safehttp.SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1")})
	t.Cleanup(func() { safehttp.SetAllowedPrivateIPs(nil) })

	full := strings.Repeat("0123456789", 20) // 200 bytes
	cacheHits := 0
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cacheHits++
		w.Header().Set("X-FetchCache-Fetched-At", time.Now().UTC().Format(time.RFC3339))
		_, _ = io.WriteString(w, full)
	}))
	defer cache.Close()
	originHits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits++
		_, _ = io.WriteString(w, full)
	}))
	defer origin.Close()

	t.Setenv(fleetfetch.EnvCacheURL, strings.Replace(cache.URL, "127.0.0.1", "localhost", 1))
	t.Setenv(fleetfetch.EnvMaxBodyBytes, "64")
	_ = server.New(&config.Config{AppName: "go_fetchcache_overlimit_test", Version: "0.0.0", Port: "0"})

	resp, err := safehttp.NewClient(safehttp.WithTimeout(2 * time.Second)).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != full {
		t.Fatalf("the caller got %d bytes, want all %d: a cut body was passed off as whole", len(body), len(full))
	}
	if cacheHits != 1 || originHits != 1 {
		t.Fatalf("cache hits %d, origin hits %d; want 1 and 1 (cache tried, then direct)", cacheHits, originHits)
	}
}
