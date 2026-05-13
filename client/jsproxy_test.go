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
		Performance: map[string]int64{"load": 123},
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
	t.Setenv("JS_PROXY_LEGACY", "")

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
	if got.Performance["load"] != 123 {
		t.Fatalf("performance not parsed: %#v", got.Performance)
	}
}

func TestJSProxy_Legacy_FallbackSynthesisesResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "k" {
			t.Errorf("expected api_key=k, got %q", r.URL.Query().Get("api_key"))
		}
		_, _ = w.Write([]byte("<html>legacy</html>"))
	}))
	defer srv.Close()

	prev := legacyJSProxyURL
	legacyJSProxyURL = srv.URL
	t.Cleanup(func() { legacyJSProxyURL = prev })

	t.Setenv("JS_PROXY_URL", "https://unused.invalid")
	t.Setenv("JS_PROXY_API_KEY", "k")
	t.Setenv("JS_PROXY_LEGACY", "true")

	got, err := JSProxy(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.DOMHTML != "<html>legacy</html>" {
		t.Fatalf("legacy DOMHTML mismatch: %q", got.DOMHTML)
	}
	if got.FinalURL != "https://example.com" {
		t.Fatalf("legacy FinalURL should fall back to target, got %q", got.FinalURL)
	}
	if got.Network != nil {
		t.Fatalf("legacy mode should leave Network nil, got %#v", got.Network)
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
	t.Setenv("JS_PROXY_LEGACY", "")

	_, err := JSProxy(context.Background(), "https://example.com")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected 502 error, got %v", err)
	}
}
