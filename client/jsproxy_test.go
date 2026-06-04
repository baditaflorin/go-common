package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/response"
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

// TestJSProxy_Network_EnvelopeAndBare is the regression guard for the
// 2026-06-04 fix: the live go-js-proxy-network service wraps its payload
// in go-common's response.Success envelope ({"status":"success",
// "data":{…}}), so decoding the body straight into a ProxyResult yielded
// an empty DOMHTML / nil Network. jsProxyNetwork now unwraps .data. The
// "bare" case keeps coverage for an un-enveloped deployment so neither
// shape regresses.
func TestJSProxy_Network_EnvelopeAndBare(t *testing.T) {
	want := ProxyResult{
		FinalURL: "https://example.com/",
		DOMHTML:  "<html><body>rendered</body></html>",
		Network: []NetworkEntry{{
			URL:          "https://example.com/api/data",
			Method:       "GET",
			Status:       200,
			ResponseSize: 1234,
			ResourceType: "xhr",
		}},
		ConsoleLogs: []string{"ready"},
		Performance: Performance{
			Lifecycle: map[string]int64{"load": 200},
			Timing:    map[string]int64{"navigationStart": 1000},
		},
	}

	cases := []struct {
		name string
		// body returns the exact bytes the server writes for `want`.
		body func() []byte
	}{
		{
			// The shape the LIVE service emits: response.Success wraps
			// ProxyResult under .data with a top-level "status":"success".
			name: "enveloped",
			body: func() []byte {
				b, _ := json.Marshal(response.Success(want))
				return b
			},
		},
		{
			// Pre-envelope / bare body: ProxyResult fields at top level.
			name: "bare",
			body: func() []byte {
				b, _ := json.Marshal(want)
				return b
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(tc.body())
			}))
			defer srv.Close()

			t.Setenv("JS_PROXY_NETWORK_URL", srv.URL)
			t.Setenv("JS_PROXY_NETWORK_API_KEY", "secret")
			t.Setenv("JS_PROXY_API_KEY", "") // force the network-specific key
			t.Setenv("JS_PROXY_URL", "")

			got, err := JSProxy(context.Background(), "https://example.com")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.DOMHTML != want.DOMHTML {
				t.Fatalf("DOMHTML not populated: got %q want %q", got.DOMHTML, want.DOMHTML)
			}
			if len(got.Network) != 1 || got.Network[0].Status != 200 || got.Network[0].URL != want.Network[0].URL {
				t.Fatalf("Network log not populated: %#v", got.Network)
			}
			if got.FinalURL != want.FinalURL {
				t.Fatalf("FinalURL mismatch: got %q want %q", got.FinalURL, want.FinalURL)
			}
			if len(got.ConsoleLogs) != 1 || got.ConsoleLogs[0] != "ready" {
				t.Fatalf("ConsoleLogs not populated: %#v", got.ConsoleLogs)
			}
			if got.Performance.Lifecycle["load"] != 200 {
				t.Fatalf("Performance not populated: %#v", got.Performance)
			}
		})
	}
}

// TestJSProxy_Network_ErrorEnvelope verifies that a 200 response carrying
// an error envelope ({"status":"error","error":{…}}) surfaces as an error
// rather than a silently-empty ProxyResult.
func TestJSProxy_Network_ErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response.NewError(422, "render.timeout", "page did not settle"))
	}))
	defer srv.Close()

	t.Setenv("JS_PROXY_NETWORK_URL", srv.URL)
	t.Setenv("JS_PROXY_NETWORK_API_KEY", "secret")
	t.Setenv("JS_PROXY_API_KEY", "")
	t.Setenv("JS_PROXY_URL", "")

	_, err := JSProxy(context.Background(), "https://example.com")
	if err == nil || !strings.Contains(err.Error(), "render.timeout") {
		t.Fatalf("expected error envelope to surface, got %v", err)
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
