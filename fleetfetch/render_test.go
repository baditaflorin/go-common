package fleetfetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureRenderParam captures the ?render= query param on every cache
// call so we can assert the client forwards it correctly.
func captureRenderParam(t *testing.T, lastRender *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*lastRender = r.URL.Query().Get("render")
		// Echo back the requested mode so the client's Response.Render
		// picks it up — mirrors the server emitting X-FetchCache-Render.
		w.Header().Set("X-FetchCache-Hit", "false")
		w.Header().Set("X-FetchCache-Final-Url", r.URL.Query().Get("url"))
		w.Header().Set("X-FetchCache-Fetched-At", "2026-05-21T00:00:00Z")
		if v := r.URL.Query().Get("render"); v != "" {
			w.Header().Set("X-FetchCache-Render", v)
			w.Header().Set("X-FetchCache-Via", "js-proxy")
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "ok")
	}))
}

func TestWithRender_DefaultSendsNoQueryParam(t *testing.T) {
	var seen string
	srv := captureRenderParam(t, &seen)
	defer srv.Close()

	c := NewClient(WithCacheURL(srv.URL))
	if _, err := c.Get(context.Background(), "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if seen != "" {
		t.Errorf("default client must not send render param; got %q", seen)
	}
}

func TestWithRender_JSForwardedAsQueryParam(t *testing.T) {
	var seen string
	srv := captureRenderParam(t, &seen)
	defer srv.Close()

	c := NewClient(WithCacheURL(srv.URL), WithRender(RenderJS))
	res, err := c.Get(context.Background(), "https://stripe.com/")
	if err != nil {
		t.Fatal(err)
	}
	if seen != "js" {
		t.Errorf("expected ?render=js on the wire, got %q", seen)
	}
	if res.Render != "js" {
		t.Errorf("Response.Render: got %q want %q", res.Render, "js")
	}
	if res.Via != "js-proxy" {
		t.Errorf("Response.Via: got %q want %q", res.Via, "js-proxy")
	}
}

func TestWithRender_PerCallOverridesClientDefault(t *testing.T) {
	// Client defaults to "js" but GetRendered("") asks for the cheap path.
	var seen string
	srv := captureRenderParam(t, &seen)
	defer srv.Close()

	c := NewClient(WithCacheURL(srv.URL), WithRender(RenderJS))
	if _, err := c.GetRendered(context.Background(), "https://stripe.com/", RenderDefault); err != nil {
		t.Fatal(err)
	}
	if seen != "" {
		t.Errorf("per-call default must override client default; got %q", seen)
	}

	if _, err := c.GetRendered(context.Background(), "https://stripe.com/", RenderHTML); err != nil {
		t.Fatal(err)
	}
	if seen != "html" {
		t.Errorf("per-call html must override client js default; got %q", seen)
	}
}
