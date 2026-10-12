package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/safehttp"
)

func TestFetchSourceMiddlewareAttachesSource(t *testing.T) {
	handler := fetchSourceMiddleware("go_test_service", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source, ok := fleetfetch.RequestSourceFromContext(r.Context())
		if !ok {
			http.Error(w, "source missing", http.StatusInternalServerError)
			return
		}
		if source == fleetfetch.SourceCommonCrawl {
			if !safehttp.RequiredFetchCacheFromContext(r.Context()) {
				http.Error(w, "archive cache requirement missing", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/?url=https://example.com/&source=commoncrawl", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusAccepted, rec.Body.String())
	}
}

func TestFetchSourceMiddlewareDoesNotRequireCacheForLiveSelftestOrCacheService(t *testing.T) {
	check := func(appName, target string) {
		t.Helper()
		handler := fetchSourceMiddleware(appName, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safehttp.RequiredFetchCacheFromContext(r.Context()) {
				http.Error(w, "unexpected required-cache marker", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("app=%s GET %s status = %d, want %d; body=%q", appName, target, rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	check("go_test_service", "/?source=live")
	check("go_test_service", "/selftest?source=commoncrawl")
	check(fetchCacheServiceAppName, "/fetch?source=commoncrawl")
}

func TestFetchSourceMiddlewareExplicitLiveOverridesDefault(t *testing.T) {
	t.Setenv(fleetfetch.EnvSource, string(fleetfetch.SourceCommonCrawl))
	handler := fetchSourceMiddleware("go_test_service", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source, ok := fleetfetch.RequestSourceFromContext(r.Context())
		if !ok || source != fleetfetch.SourceLive {
			http.Error(w, "live override missing", http.StatusInternalServerError)
			return
		}
		if safehttp.RequiredFetchCacheFromContext(r.Context()) {
			http.Error(w, "live override retained archive-only marker", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/?url=https://example.com/&source=live", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestFetchSourceMiddlewareRejectsInvalidOrRepeatedSource(t *testing.T) {
	handler := fetchSourceMiddleware("go_test_service", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, target := range []string{"/?source=javascript", "/?source=live&source=commoncrawl"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want %d", target, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestFetchSourceMiddlewareLeavesUnspecifiedSourceAlone(t *testing.T) {
	t.Setenv(fleetfetch.EnvSource, string(fleetfetch.SourceLive))
	handler := fetchSourceMiddleware("go_test_service", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := fleetfetch.RequestSourceFromContext(r.Context()); ok {
			http.Error(w, "unexpected source override", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/?url=https://example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestFetchSourceMiddlewareUsesCommonCrawlEnvironmentDefault(t *testing.T) {
	t.Setenv(fleetfetch.EnvSource, string(fleetfetch.SourceCommonCrawl))
	handler := fetchSourceMiddleware("go_test_service", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !safehttp.RequiredFetchCacheFromContext(r.Context()) {
			http.Error(w, "archive cache requirement missing", http.StatusInternalServerError)
			return
		}
		if err := safehttp.GuardHost(r.Context(), "does-not-resolve.invalid"); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodGet, "/?target=https://does-not-resolve.invalid/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusAccepted, rec.Body.String())
	}
}

func TestServerHandlerPropagatesFetchSourceToFleetFetch(t *testing.T) {
	t.Setenv(fleetfetch.EnvSource, "live")
	var seen string
	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("source")
		w.Header().Set("X-FetchCache-Final-Url", "https://example.com/")
		w.Header().Set("X-FetchCache-Fetched-At", "2026-10-09T00:00:00Z")
		w.Header().Set("X-FetchCache-Hit", "false")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "archive-or-live-body")
	}))
	defer cache.Close()

	fetch := fleetfetch.NewClient(fleetfetch.WithCacheURL(cache.URL))
	srv := New(&config.Config{AppName: "request-source-test", Version: "0.0.0", Port: "0"})
	srv.Mux.HandleFunc("/extract", func(w http.ResponseWriter, r *http.Request) {
		response, err := fetch.Get(r.Context(), "https://example.com/")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response.Body)
	})

	req := httptest.NewRequest(http.MethodGet, "/extract?url=https://example.com/&source=commoncrawl", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if seen != "commoncrawl" {
		t.Fatalf("fetch cache source = %q, want commoncrawl", seen)
	}
	if rec.Body.String() != "archive-or-live-body" {
		t.Fatalf("handler body = %q", rec.Body.String())
	}
}
