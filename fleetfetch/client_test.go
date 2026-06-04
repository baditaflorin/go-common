package fleetfetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/header"
)

func TestNewClient_DefaultsAndEnv(t *testing.T) {
	t.Setenv(EnvCacheURL, "https://override.example/")
	t.Setenv(EnvAPIKey, "secret")
	c := NewClient()
	if c.cacheURL != "https://override.example/" {
		t.Errorf("cacheURL: got %q want env override", c.cacheURL)
	}
	if c.apiKey != "secret" {
		t.Errorf("apiKey: got %q want env value", c.apiKey)
	}
}

func TestNewClient_DefaultIsInternalContainerDNS(t *testing.T) {
	// Make sure no leftover env interferes.
	t.Setenv(EnvCacheURL, "")
	t.Setenv(EnvAPIKey, "")
	c := NewClient()
	if c.cacheURL != DefaultURL {
		t.Errorf("default cacheURL: got %q want %q", c.cacheURL, DefaultURL)
	}
	if DefaultURL != "http://go_infrastructure_fetch_cache:18205" {
		t.Errorf("DefaultURL: got %q want internal container-DNS form", DefaultURL)
	}
}

func TestNewClient_DefaultAPIKeyIsDefaultToken(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	c := NewClient()
	if c.apiKey != DefaultAPIKey {
		t.Errorf("default apiKey: got %q want %q", c.apiKey, DefaultAPIKey)
	}
}

func TestGet_CacheReturns4xx_WithoutFetchCacheHeader_FallsBack(t *testing.T) {
	// Cache returns 401 (no X-FetchCache-* headers) — simulates the
	// in-process keystore rejecting an unauthenticated request.
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer cacheSrv.Close()

	originHit := 0
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHit++
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "direct after 401")
	}))
	defer originSrv.Close()

	c := NewClient(
		WithCacheURL(cacheSrv.URL),
		WithFallbackClient(&http.Client{Timeout: 5 * time.Second}),
	)
	r, err := c.Get(context.Background(), originSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ViaFallback {
		t.Error("expected ViaFallback=true on cache-side 4xx")
	}
	if string(r.Body) != "direct after 401" {
		t.Errorf("body: %q", r.Body)
	}
	if originHit != 1 {
		t.Errorf("origin hit count: %d", originHit)
	}
}

func TestGet_CacheReturns4xx_WithFetchCacheHeader_PassesThrough(t *testing.T) {
	// Cache returns 404 WITH X-FetchCache-* headers — meaning the
	// upstream returned 404 and the cache faithfully passed it through.
	// Should NOT fall back; the producer should see the real 404.
	originHit := 0
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHit++
		w.Header().Set("X-FetchCache-Hit", "false")
		w.Header().Set("X-FetchCache-Fetched-At", time.Now().UTC().Format(time.RFC3339))
		w.WriteHeader(404)
		_, _ = io.WriteString(w, "upstream 404")
	}))
	defer cacheSrv.Close()

	c := NewClient(WithCacheURL(cacheSrv.URL))
	r, err := c.Get(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if r.ViaFallback {
		t.Error("upstream 4xx should NOT trigger fallback")
	}
	if r.Status != 404 {
		t.Errorf("status: got %d want 404", r.Status)
	}
	if originHit != 1 {
		t.Errorf("cache hit count: %d (no fallback expected)", originHit)
	}
}

func TestNewClient_OptionsBeatEnv(t *testing.T) {
	t.Setenv(EnvCacheURL, "https://env.example/")
	c := NewClient(WithCacheURL("https://opt.example/"))
	if c.cacheURL != "https://opt.example/" {
		t.Fatalf("option should beat env: got %q", c.cacheURL)
	}
}

func TestGet_CacheHit(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotKey = r.Header.Get(header.APIKey)
		w.Header().Set("X-FetchCache-Hit", "true")
		w.Header().Set("X-FetchCache-Age-Seconds", "42")
		w.Header().Set("X-FetchCache-Final-Url", "https://example.com/")
		w.Header().Set("X-FetchCache-Upstream-Ms", "118")
		w.Header().Set("X-FetchCache-Fetched-At", time.Now().UTC().Format(time.RFC3339))
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>cached</html>"))
	}))
	defer srv.Close()

	c := NewClient(WithCacheURL(srv.URL), WithAPIKey("k"))
	r, err := c.Get(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != 200 {
		t.Errorf("status: %d", r.Status)
	}
	if !r.Hit {
		t.Error("expected Hit=true")
	}
	if r.AgeSeconds != 42 {
		t.Errorf("age: %d", r.AgeSeconds)
	}
	if r.UpstreamMS != 118 {
		t.Errorf("upstream_ms: %d", r.UpstreamMS)
	}
	if !strings.Contains(string(r.Body), "cached") {
		t.Errorf("body: %q", r.Body)
	}
	if !strings.Contains(gotPath, "/fetch?") || !strings.Contains(gotPath, "url=https") {
		t.Errorf("path didn't include encoded target: %q", gotPath)
	}
	if gotKey != "k" {
		t.Errorf("API key not forwarded: %q", gotKey)
	}
	if got := c.Stats(); got.Hits != 1 || got.Misses != 0 {
		t.Errorf("stats: %+v", got)
	}
}

func TestGet_MissCountsCorrectly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-FetchCache-Hit", "false")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := NewClient(WithCacheURL(srv.URL))
	if _, err := c.Get(context.Background(), "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.Misses != 1 || s.Hits != 0 {
		t.Errorf("stats: %+v", s)
	}
}

func TestGet_CacheReturns5xx_FallsBackToDirect(t *testing.T) {
	// Cache always 503.
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer cacheSrv.Close()

	// Origin returns real content.
	originHit := 0
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHit++
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "direct from origin")
	}))
	defer originSrv.Close()

	// Override fallback with a plain client so we can target the
	// origin testserver (safehttp blocks 127.0.0.1 by default).
	c := NewClient(
		WithCacheURL(cacheSrv.URL),
		WithFallbackClient(&http.Client{Timeout: 5 * time.Second}),
	)
	r, err := c.Get(context.Background(), originSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ViaFallback {
		t.Error("expected ViaFallback=true")
	}
	if string(r.Body) != "direct from origin" {
		t.Errorf("body: %q", r.Body)
	}
	if originHit != 1 {
		t.Errorf("origin hit count: %d", originHit)
	}
	if s := c.Stats(); s.Fallbacks != 1 {
		t.Errorf("stats: %+v", s)
	}
}

func TestGet_CacheTimeout_DoesNotFallBackByDefault(t *testing.T) {
	// Cache is reachable but slower than the client timeout.
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer cacheSrv.Close()

	originHit := 0
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHit++
		w.WriteHeader(200)
	}))
	defer originSrv.Close()

	c := NewClient(
		WithCacheURL(cacheSrv.URL),
		WithTimeout(40*time.Millisecond),
		WithFallbackClient(&http.Client{Timeout: 5 * time.Second}),
	)
	_, err := c.Get(context.Background(), originSrv.URL)
	if err == nil {
		t.Fatal("expected ErrCacheTimeout, got nil")
	}
	if !errors.Is(err, ErrCacheTimeout) {
		t.Fatalf("expected ErrCacheTimeout, got %v", err)
	}
	if originHit != 0 {
		t.Errorf("must NOT direct-fetch origin on timeout by default; origin hit %d times", originHit)
	}
	if s := c.Stats(); s.Timeouts != 1 || s.Fallbacks != 0 {
		t.Errorf("stats: %+v (want Timeouts=1, Fallbacks=0)", s)
	}
}

func TestGet_CacheTimeout_FallsBackWhenOptedIn(t *testing.T) {
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer cacheSrv.Close()

	originHit := 0
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHit++
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "direct after timeout")
	}))
	defer originSrv.Close()

	c := NewClient(
		WithCacheURL(cacheSrv.URL),
		WithTimeout(40*time.Millisecond),
		WithFallbackOnTimeout(),
		WithFallbackClient(&http.Client{Timeout: 5 * time.Second}),
	)
	r, err := c.Get(context.Background(), originSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ViaFallback {
		t.Error("expected ViaFallback=true when WithFallbackOnTimeout is set")
	}
	if string(r.Body) != "direct after timeout" {
		t.Errorf("body: %q", r.Body)
	}
	if originHit != 1 {
		t.Errorf("origin hit count: %d", originHit)
	}
	if s := c.Stats(); s.Timeouts != 1 || s.Fallbacks != 1 {
		t.Errorf("stats: %+v (want Timeouts=1, Fallbacks=1)", s)
	}
}

func TestGet_EmptyURL(t *testing.T) {
	c := NewClient()
	if _, err := c.Get(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty url")
	}
}

func TestGetWithMaxAge_EmitsMaxAgeParam(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.RawQuery
		w.Header().Set("X-FetchCache-Hit", "false")
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := NewClient(WithCacheURL(srv.URL))
	_, err := c.GetWithMaxAge(context.Background(), "https://example.com/", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotURL, "max_age=30") {
		t.Errorf("missing max_age=30 in query: %q", gotURL)
	}
}

// TestNewHTTPClient_SlowCacheFallsBackNot502 is a regression for the bug
// where NewHTTPClient set http.Client.Timeout == the per-fetch timeout, so
// the outer deadline cancelled the request context at the exact moment the
// cache attempt timed out — leaving no budget for the WithFallbackOnTimeout
// direct fetch. The slow cache must degrade to a 200 from the fallback, not
// a hard error.
func TestNewHTTPClient_SlowCacheFallsBackNot502(t *testing.T) {
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond) // past the 200ms per-fetch timeout
		w.Header().Set("X-FetchCache-Hit", "false")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "slow cache body")
	}))
	defer cacheSrv.Close()

	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "direct fallback body")
	}))
	defer originSrv.Close()

	hc := NewHTTPClient(
		WithCacheURL(cacheSrv.URL),
		WithFallbackClient(&http.Client{Timeout: 5 * time.Second}),
		WithFallbackOnTimeout(),
		WithTimeout(200*time.Millisecond),
	)
	resp, err := hc.Get(originSrv.URL)
	if err != nil {
		t.Fatalf("slow cache must fall back, got error (the 502 bug): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "direct fallback body" {
		t.Fatalf("body: got %q want %q", body, "direct fallback body")
	}
	if resp.Header.Get("X-FetchCache-Via-Fallback") != "true" {
		t.Errorf("expected X-FetchCache-Via-Fallback=true")
	}
}

// TestWithoutCache_BypassesCacheEntirely locks the WithoutCache contract:
// the client must NEVER call the cache URL and must direct-fetch the origin
// instead — for services probing speculative URLs not worth caching.
func TestWithoutCache_BypassesCacheEntirely(t *testing.T) {
	var cacheHits int
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cacheHits++
		w.WriteHeader(500)
	}))
	defer cacheSrv.Close()

	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "direct origin body")
	}))
	defer originSrv.Close()

	c := NewClient(
		WithCacheURL(cacheSrv.URL),
		WithoutCache(),
		// Plain fallback client so the direct fetch can reach the
		// 127.0.0.1 httptest origin (safehttp would SSRF-block it).
		WithFallbackClient(&http.Client{Timeout: 5 * time.Second}),
	)
	r, err := c.Get(context.Background(), originSrv.URL)
	if err != nil {
		t.Fatalf("WithoutCache Get: %v", err)
	}
	if cacheHits != 0 {
		t.Fatalf("cache must NOT be contacted under WithoutCache; got %d hits", cacheHits)
	}
	if r.Status != 200 {
		t.Fatalf("status: got %d want 200", r.Status)
	}
	if string(r.Body) != "direct origin body" {
		t.Fatalf("body: got %q want %q", r.Body, "direct origin body")
	}
	if !r.ViaFallback {
		t.Error("expected ViaFallback=true on the direct path")
	}
}

// captureObserver records every fetch Event for assertions.
type captureObserver struct{ events []Event }

func (c *captureObserver) ObserveFleetFetch(e Event) { c.events = append(c.events, e) }

// TestWithoutCache_EmitsDirectResultLabels locks the observability contract:
// WithoutCache fetches must emit result="direct" (success) / "direct_error"
// (direct fetch failed) — NEVER "fallback"/"error"/"hit" — so by-design
// cache-bypass traffic is distinguishable from real cache activity.
func TestWithoutCache_EmitsDirectResultLabels(t *testing.T) {
	obs := &captureObserver{}
	SetDefaultObserver(obs)
	defer SetDefaultObserver(nil)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	deadURL := origin.URL

	c := NewClient(
		WithCacheURL("http://cache.invalid"), // must never be contacted
		WithoutCache(),
		WithFallbackClient(&http.Client{Timeout: 3 * time.Second}),
	)

	// 1) successful direct fetch → result="direct"
	if _, err := c.Get(context.Background(), origin.URL); err != nil {
		t.Fatalf("direct success Get: %v", err)
	}
	// 2) close origin, refetch → connection refused → result="direct_error"
	origin.Close()
	if _, err := c.Get(context.Background(), deadURL); err == nil {
		t.Fatal("expected direct fetch to fail against a closed origin")
	}

	if len(obs.events) != 2 {
		t.Fatalf("want 2 emitted events, got %d (%+v)", len(obs.events), obs.events)
	}
	if obs.events[0].Result != "direct" {
		t.Errorf("success event: got result=%q, want %q", obs.events[0].Result, "direct")
	}
	if obs.events[1].Result != "direct_error" {
		t.Errorf("failure event: got result=%q, want %q", obs.events[1].Result, "direct_error")
	}
}
