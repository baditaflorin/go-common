package fleetfetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/loadshed"
)

func TestLoadShed_DefaultFallsBackButRenderedReturnsTypedBusy(t *testing.T) {
	tests := []struct {
		name         string
		render       string
		wantFallback bool
	}{
		{"default", RenderDefault, true},
		{"js", RenderJS, false},
		{"html", RenderHTML, false},
		{"js-network", RenderJSNetwork, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var originHits atomic.Int64
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				originHits.Add(1)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "direct origin")
			}))
			defer origin.Close()

			cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				loadshed.WriteShed(w, 7, "renderer saturated")
			}))
			defer cache.Close()

			client := NewClient(
				WithCacheURL(cache.URL),
				WithRender(tc.render),
				WithFallbackOnTimeout(), // must not alter explicit shed behavior
				WithFallbackClient(origin.Client()),
			)
			response, err := client.Get(context.Background(), origin.URL)

			if tc.wantFallback {
				if err != nil {
					t.Fatalf("default fetch should preserve direct fallback: %v", err)
				}
				if response == nil || !response.ViaFallback || string(response.Body) != "direct origin" {
					t.Fatalf("unexpected fallback response: %+v", response)
				}
				if originHits.Load() != 1 {
					t.Fatalf("origin hits=%d want 1", originHits.Load())
				}
				return
			}

			if response != nil {
				t.Fatalf("render shed must not return raw fallback bytes: %+v", response)
			}
			if !errors.Is(err, ErrRenderBusy) {
				t.Fatalf("errors.Is(ErrRenderBusy)=false: %v", err)
			}
			var busy *RenderBusyError
			if !errors.As(err, &busy) {
				t.Fatalf("errors.As(*RenderBusyError)=false: %T %v", err, err)
			}
			if busy.StatusCode != http.StatusServiceUnavailable || busy.RetryAfter != "7" || busy.Message != "renderer saturated" {
				t.Fatalf("busy details: %+v", busy)
			}
			if delay, ok := busy.RetryDelay(time.Now()); !ok || delay != 7*time.Second {
				t.Fatalf("retry delay=%s ok=%v", delay, ok)
			}
			if originHits.Load() != 0 {
				t.Fatalf("render shed must not direct-fetch; origin hits=%d", originHits.Load())
			}
			if stats := client.Stats(); stats.Fallbacks != 0 || stats.Errors != 1 {
				t.Fatalf("stats=%+v want no fallback and one error", stats)
			}
		})
	}
}

func TestRenderBusyError_RetryDelayHTTPDate(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	err := &RenderBusyError{RetryAfter: now.Add(9 * time.Second).Format(http.TimeFormat)}
	if delay, ok := err.RetryDelay(now); !ok || delay != 9*time.Second {
		t.Fatalf("retry delay=%s ok=%v", delay, ok)
	}
	err.RetryAfter = "invalid"
	if _, ok := err.RetryDelay(now); ok {
		t.Fatal("invalid Retry-After must not parse")
	}
}

func TestNewHTTPClient_RenderShedPreservesTypedError(t *testing.T) {
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		loadshed.WriteShed(w, 4, "browser pool full")
	}))
	defer cache.Close()

	client := NewHTTPClient(
		WithCacheURL(cache.URL),
		WithRender(RenderJS),
		WithFallbackClient(cache.Client()),
	)
	response, err := client.Get("https://example.com")
	if response != nil {
		response.Body.Close()
		t.Fatalf("render shed unexpectedly returned an HTTP response: %+v", response)
	}
	if !errors.Is(err, ErrRenderBusy) {
		t.Fatalf("drop-in HTTP client lost ErrRenderBusy: %T %v", err, err)
	}
	var busy *RenderBusyError
	if !errors.As(err, &busy) || busy.RetryAfter != "4" {
		t.Fatalf("drop-in HTTP client lost Retry-After: %+v", busy)
	}
}

func TestCachedOrigin5xxPassesThroughWithoutDirectFallback(t *testing.T) {
	tests := []struct {
		name   string
		status int
		render string
		body   string
	}{
		{"default-500", http.StatusInternalServerError, RenderDefault, "cached origin 500"},
		{"default-503", http.StatusServiceUnavailable, RenderDefault, "cached origin 503"},
		{"js-500", http.StatusInternalServerError, RenderJS, "cached origin 500"},
		// Even a body resembling a shed response is an origin replay when the
		// cache supplies Fetched-At; provenance wins over body classification.
		{"js-cached-shed-shape", http.StatusServiceUnavailable, RenderJS, `{"status":"error","error":{"code":503,"error_code":"load_shed","message":"origin body"}}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var originHits atomic.Int64
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				originHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer origin.Close()

			cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-FetchCache-Hit", "true")
				w.Header().Set("X-FetchCache-Fetched-At", "2026-08-04T12:00:00Z")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer cache.Close()

			client := NewClient(
				WithCacheURL(cache.URL),
				WithRender(tc.render),
				WithFallbackClient(origin.Client()),
			)
			response, err := client.Get(context.Background(), origin.URL)
			if err != nil {
				t.Fatalf("cached upstream 5xx returned error: %v", err)
			}
			if response.Status != tc.status || string(response.Body) != tc.body || response.ViaFallback || !response.Hit {
				t.Fatalf("unexpected cached response: %+v body=%q", response, response.Body)
			}
			if originHits.Load() != 0 {
				t.Fatalf("negative cache was bypassed; origin hits=%d", originHits.Load())
			}
			if stats := client.Stats(); stats.Hits != 1 || stats.Fallbacks != 0 {
				t.Fatalf("stats=%+v", stats)
			}
		})
	}
}

func TestGenuineCacheFailureStillFallsBackForAllRenderModes(t *testing.T) {
	tests := []struct {
		name   string
		render string
		kind   string
	}{
		{"default-transport", RenderDefault, "transport"},
		{"js-transport", RenderJS, "transport"},
		{"js-network-transport", RenderJSNetwork, "transport"},
		{"default-untyped-503", RenderDefault, "503"},
		{"js-untyped-503", RenderJS, "503"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "fallback survived")
			}))
			defer origin.Close()

			cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, "ordinary cache failure")
			}))
			cacheURL := cache.URL
			if tc.kind == "transport" {
				cache.Close()
			} else {
				defer cache.Close()
			}

			client := NewClient(
				WithCacheURL(cacheURL),
				WithRender(tc.render),
				WithFallbackClient(origin.Client()),
			)
			response, err := client.Get(context.Background(), origin.URL)
			if err != nil {
				t.Fatalf("genuine cache failure lost backward-compatible fallback: %v", err)
			}
			if !response.ViaFallback || string(response.Body) != "fallback survived" {
				t.Fatalf("unexpected response: %+v body=%q", response, response.Body)
			}
		})
	}
}

func TestFetchCacheHopHeaderIsControlMetadataNotOriginIdentity(t *testing.T) {
	var cacheHop, forwardedHop, forwardedOrdinary string
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cacheHop = r.Header.Get(fetchCacheHopHeader)
		forwardedHop = r.Header.Get(ForwardHeaderPrefix + fetchCacheHopHeader)
		forwardedOrdinary = r.Header.Get(ForwardHeaderPrefix + "X-Representation")
		w.Header().Set("X-FetchCache-Fetched-At", "2026-08-04T12:00:00Z")
		w.WriteHeader(http.StatusOK)
	}))
	defer cache.Close()

	client := NewClient(WithCacheURL(cache.URL))
	_, err := client.GetWithHeaders(context.Background(), "https://example.com", http.Header{
		fetchCacheHopHeader: []string{"1"},
		"X-Representation":  []string{"mobile"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cacheHop != "1" || forwardedHop != "" || forwardedOrdinary != "mobile" {
		t.Fatalf("cache headers: direct-hop=%q forwarded-hop=%q ordinary=%q", cacheHop, forwardedHop, forwardedOrdinary)
	}

	var originHop, originOrdinary string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHop = r.Header.Get(fetchCacheHopHeader)
		originOrdinary = r.Header.Get("X-Representation")
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	deadCache := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := deadCache.URL
	deadCache.Close()
	client = NewClient(WithCacheURL(deadURL), WithFallbackClient(origin.Client()))
	_, err = client.GetWithHeaders(context.Background(), origin.URL, http.Header{
		fetchCacheHopHeader: []string{"1"},
		"X-Representation":  []string{"mobile"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if originHop != "" || originOrdinary != "mobile" {
		t.Fatalf("origin headers: hop=%q ordinary=%q", originHop, originOrdinary)
	}
}
