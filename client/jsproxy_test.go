package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSProxy_RequiresTarget(t *testing.T) {
	t.Setenv("JS_PROXY_URL", "https://example.invalid")
	t.Setenv("JS_PROXY_API_KEY", "k")
	_, err := JSProxy(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestJSProxy_RequiresAPIKey(t *testing.T) {
	t.Setenv("JS_PROXY_URL", "https://example.invalid")
	t.Setenv("JS_PROXY_API_KEY", "")
	_, err := JSProxy(context.Background(), "https://example.com")
	if err == nil {
		t.Fatal("expected error when JS_PROXY_API_KEY is unset")
	}
}

func TestJSProxy_Modern_ParsesResponse(t *testing.T) {
	want := ProxyResult{
		FinalURL: "https://example.com/",
		DOMHTML:  "<html></html>",
		Network: []NetworkEntry{{
			URL:          "https://example.com/",
			Method:       "GET",
			Status:       200,
			ResourceType: "document",
		}},
		ConsoleLogs: []string{"hello"},
		Performance: Performance{
			Lifecycle: map[string]int64{"load": 123, "firstPaint": 50},
			Timing:    map[string]int64{"navigationStart": 1000, "loadEventEnd": 1123},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "secret" {
			t.Errorf("expected api_key=secret in query, got %q", r.URL.Query().Get("api_key"))
		}
		if r.URL.Query().Get("url") != "https://example.com" {
			t.Errorf("expected url=example.com, got %q", r.URL.Query().Get("url"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	t.Setenv("JS_PROXY_URL", srv.URL)
	t.Setenv("JS_PROXY_API_KEY", "secret")

	got, err := JSProxy(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.DOMHTML != want.DOMHTML {
		t.Fatalf("DOMHTML mismatch: got %q want %q", got.DOMHTML, want.DOMHTML)
	}
	if len(got.Network) != 1 || got.Network[0].Status != 200 {
		t.Fatalf("network log not parsed correctly: %#v", got.Network)
	}
	if got.Performance.Lifecycle["load"] != 123 {
		t.Fatalf("performance.lifecycle.load not parsed: %#v", got.Performance.Lifecycle)
	}
	if got.Performance.Lifecycle["firstPaint"] != 50 {
		t.Fatalf("performance.lifecycle.firstPaint not parsed: %#v", got.Performance.Lifecycle)
	}
	if got.Performance.Timing["navigationStart"] != 1000 {
		t.Fatalf("performance.timing.navigationStart not parsed: %#v", got.Performance.Timing)
	}
	if got.Performance.Timing["loadEventEnd"] != 1123 {
		t.Fatalf("performance.timing.loadEventEnd not parsed: %#v", got.Performance.Timing)
	}
}

func TestJSProxyDOM_SynthesisesResult(t *testing.T) {
	// go-js-proxy (DOM-only) returns raw HTML, not JSON. The client
	// wraps it into a ProxyResult with Network/ConsoleLogs/Performance
	// left nil so callers can tell the two paths apart by inspection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "k" {
			t.Errorf("expected api_key=k, got %q", r.URL.Query().Get("api_key"))
		}
		_, _ = w.Write([]byte("<html>dom-only</html>"))
	}))
	defer srv.Close()

	t.Setenv("JS_PROXY_DOM_URL", srv.URL)
	t.Setenv("JS_PROXY_API_KEY", "k")

	got, err := JSProxyDOM(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.DOMHTML != "<html>dom-only</html>" {
		t.Fatalf("dom-only DOMHTML mismatch: %q", got.DOMHTML)
	}
	if got.FinalURL != "https://example.com" {
		t.Fatalf("dom-only FinalURL should fall back to target, got %q", got.FinalURL)
	}
	if got.Network != nil {
		t.Fatalf("dom-only path should leave Network nil, got %#v", got.Network)
	}
}

func TestJSProxy_NetworkAndDOM_UseSeparateKeys(t *testing.T) {
	// Verify each proxy reads its OWN key, not the other's. The
	// back-compat JS_PROXY_API_KEY is *not* set; only the specific
	// ones are.
	netSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "net-key" {
			t.Errorf("network proxy got wrong key: %q", r.URL.Query().Get("api_key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"final_url":"https://x","dom_html":"<html/>"}`))
	}))
	defer netSrv.Close()

	domSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "dom-key" {
			t.Errorf("dom proxy got wrong key: %q", r.URL.Query().Get("api_key"))
		}
		_, _ = w.Write([]byte(`<html/>`))
	}))
	defer domSrv.Close()

	t.Setenv("JS_PROXY_NETWORK_URL", netSrv.URL)
	t.Setenv("JS_PROXY_DOM_URL", domSrv.URL)
	t.Setenv("JS_PROXY_NETWORK_API_KEY", "net-key")
	t.Setenv("JS_PROXY_DOM_API_KEY", "dom-key")
	t.Setenv("JS_PROXY_API_KEY", "") // explicitly NOT set
	t.Setenv("JS_PROXY_URL", "")

	if _, err := JSProxy(context.Background(), "https://x"); err != nil {
		t.Fatalf("network proxy with its own key should work: %v", err)
	}
	if _, err := JSProxyDOM(context.Background(), "https://x"); err != nil {
		t.Fatalf("dom proxy with its own key should work: %v", err)
	}
}

func TestJSProxy_LegacyKey_FallbackForBoth(t *testing.T) {
	// Back-compat: when only JS_PROXY_API_KEY is set, both proxies
	// should still authenticate. (Pre-split deployments.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "legacy-key" {
			t.Errorf("expected legacy-key fallback, got %q", r.URL.Query().Get("api_key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"final_url":"https://x","dom_html":"<html/>"}`))
	}))
	defer srv.Close()

	t.Setenv("JS_PROXY_NETWORK_URL", srv.URL)
	t.Setenv("JS_PROXY_DOM_URL", srv.URL)
	t.Setenv("JS_PROXY_NETWORK_API_KEY", "")
	t.Setenv("JS_PROXY_DOM_API_KEY", "")
	t.Setenv("JS_PROXY_API_KEY", "legacy-key")

	if _, err := JSProxy(context.Background(), "https://x"); err != nil {
		t.Fatalf("network proxy should fall back to JS_PROXY_API_KEY: %v", err)
	}
	if _, err := JSProxyDOM(context.Background(), "https://x"); err != nil {
		t.Fatalf("dom proxy should fall back to JS_PROXY_API_KEY: %v", err)
	}
}

func TestJSProxy_Modern_BadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()

	t.Setenv("JS_PROXY_URL", srv.URL)
	t.Setenv("JS_PROXY_API_KEY", "secret")

	_, err := JSProxy(context.Background(), "https://example.com")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected 502 error, got %v", err)
	}
}
