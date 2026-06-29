package fleetfetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/header"
)

func TestSanitizeCaller(t *testing.T) {
	cases := map[string]string{
		"go_founding_year":    "go_founding_year",
		"  go-amp-detector  ": "go-amp-detector",
		"evil\"name\nhere":    "evil_name_here",
		"!!!":                 "", // no alphanumeric → no signal
		"":                    "",
		"svc.0exec.com:18006": "svc.0exec.com:18006",
	}
	for in, want := range cases {
		if got := sanitizeCaller(in); got != want {
			t.Errorf("sanitizeCaller(%q) = %q, want %q", in, got, want)
		}
	}
	// length cap
	long := ""
	for i := 0; i < 200; i++ {
		long += "a"
	}
	if got := sanitizeCaller(long); len(got) != 64 {
		t.Errorf("length cap: got %d, want 64", len(got))
	}
}

func TestResolveCallerPriority(t *testing.T) {
	// reset process default around the test
	SetDefaultCaller("")
	defer SetDefaultCaller("")

	// 1. nothing set → ""
	c := NewClient(WithCacheURL("https://x/"))
	if got := c.resolveCaller(); got != "" {
		t.Fatalf("no source: got %q, want empty", got)
	}

	// 2. env fallback
	t.Setenv("FLEET_SERVICE_ID", "go_from_env")
	if got := c.resolveCaller(); got != "go_from_env" {
		t.Fatalf("env: got %q", got)
	}

	// 3. process default beats env
	SetDefaultCaller("go_from_default")
	if got := c.resolveCaller(); got != "go_from_default" {
		t.Fatalf("default: got %q", got)
	}

	// 4. per-client WithCaller beats everything
	c2 := NewClient(WithCacheURL("https://x/"), WithCaller("go_explicit"))
	if got := c2.resolveCaller(); got != "go_explicit" {
		t.Fatalf("WithCaller: got %q", got)
	}
}

// The cache request must carry X-Fleet-Caller so the cache can forward it
// to go-js-proxy for per-enricher attribution.
func TestFetchSendsFleetCallerToCache(t *testing.T) {
	SetDefaultCaller("")
	defer SetDefaultCaller("")

	var gotCaller string
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCaller = r.Header.Get(header.FleetCaller)
		w.Header().Set("X-FetchCache-Hit", "false")
		w.Header().Set("X-FetchCache-Fetched-At", time.Now().UTC().Format(time.RFC3339))
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	}))
	defer cacheSrv.Close()

	c := NewClient(WithCacheURL(cacheSrv.URL), WithCaller("go_founding_year"))
	if _, err := c.Get(context.Background(), "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if gotCaller != "go_founding_year" {
		t.Fatalf("cache saw X-Fleet-Caller=%q, want go_founding_year", gotCaller)
	}
}

// The direct-to-origin fallback must NOT leak X-Fleet-Caller to the public
// origin — it's an internal-only header for the cache hop.
func TestFallbackDoesNotLeakFleetCaller(t *testing.T) {
	SetDefaultCaller("go_should_not_leak")
	defer SetDefaultCaller("")

	// Cache rejects with a bare 401 (no X-FetchCache-* headers) → fallback.
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer cacheSrv.Close()

	var originSawCaller string
	originSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originSawCaller = r.Header.Get(header.FleetCaller)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("direct"))
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
		t.Fatal("expected fallback path")
	}
	if originSawCaller != "" {
		t.Fatalf("origin leaked X-Fleet-Caller=%q, want none", originSawCaller)
	}
}
