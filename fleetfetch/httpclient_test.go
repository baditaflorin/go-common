package fleetfetch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Stand up a stub cache server, build a fleetfetch HTTP client pointed
// at it, and assert standard *http.Client semantics still hold.
func newStubCache(t *testing.T, body string, status int, captureRender *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captureRender != nil {
			*captureRender = r.URL.Query().Get("render")
		}
		w.Header().Set("X-FetchCache-Hit", "false")
		w.Header().Set("X-FetchCache-Final-Url", r.URL.Query().Get("url"))
		w.Header().Set("X-FetchCache-Fetched-At", "2026-05-21T00:00:00Z")
		if v := r.URL.Query().Get("render"); v != "" {
			w.Header().Set("X-FetchCache-Render", v)
			w.Header().Set("X-FetchCache-Via", "js-proxy")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func TestNewHTTPClient_GetReturnsHTTPResponse(t *testing.T) {
	srv := newStubCache(t, "<html>cached</html>", 200, nil)
	defer srv.Close()

	client := NewHTTPClient(WithCacheURL(srv.URL))
	resp, err := client.Get("https://example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "cached") {
		t.Errorf("body: got %q", string(body))
	}
}

func TestNewHTTPClient_WithRenderForwardsParam(t *testing.T) {
	var seen string
	srv := newStubCache(t, "<html>js</html>", 200, &seen)
	defer srv.Close()

	client := NewHTTPClient(WithCacheURL(srv.URL), WithRender(RenderJS))
	resp, err := client.Get("https://stripe.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if seen != "js" {
		t.Errorf("expected ?render=js on the wire, got %q", seen)
	}
	if got := resp.Header.Get("X-FetchCache-Render"); got != "js" {
		t.Errorf("X-FetchCache-Render: got %q want js", got)
	}
	if got := resp.Header.Get("X-FetchCache-Via"); got != "js-proxy" {
		t.Errorf("X-FetchCache-Via: got %q want js-proxy", got)
	}
}

func TestNewHTTPClient_FinalURLOnResponse(t *testing.T) {
	srv := newStubCache(t, "", 200, nil)
	defer srv.Close()

	client := NewHTTPClient(WithCacheURL(srv.URL))
	resp, err := client.Get("https://example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	// The adapter should surface the cache's X-FetchCache-Final-Url
	// on resp.Request.URL so existing consumer code that reads
	// resp.Request.URL.String() (typical after-redirect pattern) sees
	// the canonical final URL.
	got := resp.Request.URL.String()
	if got != "https://example.com" {
		t.Errorf("Request.URL: got %q want https://example.com", got)
	}
}

func TestNewHTTPClient_NonGETPassesThrough(t *testing.T) {
	// A POST should NOT go through the cache (cache is GET-only).
	// We verify by pointing the client at a cache URL that would 500
	// on POST, and asserting the client tries to hit an origin URL
	// instead. The origin returns 418 so we can distinguish.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer origin.Close()

	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer cacheSrv.Close()

	client := NewHTTPClient(WithCacheURL(cacheSrv.URL))
	req, _ := http.NewRequest(http.MethodPost, origin.URL, strings.NewReader("hi"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("non-GET should pass through to origin; got %d want 418", resp.StatusCode)
	}
}

func TestNewHTTPClient_HeadersForwarded(t *testing.T) {
	var sawAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Forwarded headers arrive with the X-Ff-Forward- prefix.
		sawAccept = r.Header.Get("X-Ff-Forward-Accept")
		w.Header().Set("X-FetchCache-Hit", "false")
		w.Header().Set("X-FetchCache-Final-Url", r.URL.Query().Get("url"))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	client := NewHTTPClient(WithCacheURL(srv.URL))
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	req.Header.Set("Accept", "application/rdap+json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if sawAccept != "application/rdap+json" {
		t.Errorf("Accept not forwarded; saw %q", sawAccept)
	}
}
