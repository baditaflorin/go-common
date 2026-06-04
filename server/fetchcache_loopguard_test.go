package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/safehttp"
)

func TestWithHopHeader(t *testing.T) {
	got := withHopHeader(nil)
	if got.Get(fetchCacheHopHeader) != "1" {
		t.Fatalf("nil input: hop header = %q, want 1", got.Get(fetchCacheHopHeader))
	}
	orig := http.Header{"X-Foo": []string{"bar"}}
	got = withHopHeader(orig)
	if got.Get("X-Foo") != "bar" || got.Get(fetchCacheHopHeader) != "1" {
		t.Fatalf("clone: got %v", got)
	}
	if orig.Get(fetchCacheHopHeader) != "" {
		t.Fatalf("original header map was mutated: %v", orig)
	}
}

func TestTargetIsCacheHost(t *testing.T) {
	t.Setenv("FLEET_FETCH_CACHE_URL", "http://go_infrastructure_fetch_cache:18205")
	cases := []struct {
		target string
		want   bool
	}{
		{"http://go_infrastructure_fetch_cache:18205/fetch?url=x", true},
		{"https://go_infrastructure_fetch_cache/health", true},
		{"https://example.com/", false},
		{"::::not a url", false},
	}
	for _, c := range cases {
		if got := targetIsCacheHost(c.target); got != c.want {
			t.Errorf("targetIsCacheHost(%q) = %v, want %v", c.target, got, c.want)
		}
	}
	t.Setenv("FLEET_FETCH_CACHE_URL", "")
	if targetIsCacheHost("http://go_infrastructure_fetch_cache:18205/fetch") {
		t.Error("unset FLEET_FETCH_CACHE_URL should disable skip-self")
	}
}

func TestFetchGet_SkipsCacheHost(t *testing.T) {
	t.Setenv("FLEET_FETCH_CACHE_URL", "http://go_infrastructure_fetch_cache:18205")
	d := fetchCacheDelegate{c: fleetfetch.NewClient()}
	if _, err := d.FetchGet(context.Background(), "http://go_infrastructure_fetch_cache:18205/fetch?url=x", nil); err == nil {
		t.Fatal("expected skip-self error for a cache-host target")
	}
}

// recordingDelegate notes whether it was asked to serve a GET.
type recordingDelegate struct{ called bool }

func (d *recordingDelegate) FetchGet(_ context.Context, _ string, _ http.Header) (*safehttp.FetchResult, error) {
	d.called = true
	return &safehttp.FetchResult{Status: 599, Body: []byte("from-delegate")}, nil
}

// TestWrapDefaults_LoopGuard: an inbound request carrying the one-hop marker
// must disable fetch-cache delegation for safehttp GETs made while handling
// it (loop broken at one hop); a request without it routes normally.
func TestWrapDefaults_LoopGuard(t *testing.T) {
	del := &recordingDelegate{}
	safehttp.SetDefaultFetchDelegate(del)
	t.Cleanup(func() { safehttp.SetDefaultFetchDelegate(nil) })

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := safehttp.NewClient(safehttp.WithTimeout(2 * time.Second))
		// example.invalid never resolves: on the direct path c.Do errors,
		// which is fine — the assertion is only on whether the delegate ran.
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://example.invalid/", nil)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
		}
		w.WriteHeader(http.StatusOK)
	})

	s := &Server{Mux: http.NewServeMux()}
	s.Mux.Handle("/x", handler)
	wrapped := s.wrapDefaults(s.Mux)

	// Without the hop header: delegate routes the GET.
	del.called = false
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !del.called {
		t.Error("without hop header: delegate was NOT called, want routed")
	}

	// With the hop header: delegate must be bypassed.
	del.called = false
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(fetchCacheHopHeader, "1")
	wrapped.ServeHTTP(httptest.NewRecorder(), req)
	if del.called {
		t.Error("with hop header: delegate was called, want bypassed (loop guard)")
	}
}
