package fleetfetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		gotKey = r.Header.Get("X-API-Key")
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
