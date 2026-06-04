package fleetfetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// networkCacheStub mimics go_infrastructure_fetch_cache v0.3+ serving the
// js-network render mode: it returns the rendered DOM as the body and the
// outbound network log on the X-FetchCache-Network header. It records the
// ?render= it saw so the test can assert the wire param.
func networkCacheStub(t *testing.T, dom, networkJSON string, lastRender *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*lastRender = r.URL.Query().Get("render")
		w.Header().Set("X-FetchCache-Hit", "false")
		w.Header().Set("X-FetchCache-Final-Url", r.URL.Query().Get("url"))
		w.Header().Set("X-FetchCache-Fetched-At", "2026-06-04T00:00:00Z")
		w.Header().Set("X-FetchCache-Render", "js-network")
		w.Header().Set("X-FetchCache-Via", "js-proxy-network")
		if networkJSON != "" {
			w.Header().Set(NetworkHeader, networkJSON)
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, dom)
	}))
}

func TestFetchNetwork_RequestsJSNetworkAndParsesLog(t *testing.T) {
	const dom = "<html><body>rendered</body></html>"
	// A realistic GA-using page's slim log: GTM loader + GA collect beacon.
	const netJSON = `[` +
		`{"url":"https://www.nytimes.com/","method":"GET","status":200,"resource_type":"Document","initiator":"other"},` +
		`{"url":"https://www.googletagmanager.com/gtm.js?id=GTM-P528B3","method":"GET","status":200,"resource_type":"Script","initiator":"parser"},` +
		`{"url":"https://www.google-analytics.com/g/collect?tid=G-XXXX&en=page_view","method":"POST","status":204,"resource_type":"XHR","initiator":"script"}` +
		`]`

	var seenRender string
	srv := networkCacheStub(t, dom, netJSON, &seenRender)
	defer srv.Close()

	c := NewClient(WithCacheURL(srv.URL))
	resp, net, err := c.FetchNetwork(context.Background(), "https://www.nytimes.com/")
	if err != nil {
		t.Fatalf("FetchNetwork: %v", err)
	}
	// Wire: must request render=js-network.
	if seenRender != "js-network" {
		t.Errorf("expected ?render=js-network on the wire, got %q", seenRender)
	}
	// DOM is in the body.
	if string(resp.Body) != dom {
		t.Errorf("Response.Body should be the rendered DOM, got %q", string(resp.Body))
	}
	if resp.Render != "js-network" {
		t.Errorf("Response.Render: got %q want js-network", resp.Render)
	}
	if resp.Via != "js-proxy-network" {
		t.Errorf("Response.Via: got %q want js-proxy-network", resp.Via)
	}
	// Network log parsed into typed entries.
	if len(net) != 3 {
		t.Fatalf("expected 3 network entries, got %d", len(net))
	}
	gtm, gaCollect := false, false
	for _, e := range net {
		switch {
		case e.URL == "https://www.googletagmanager.com/gtm.js?id=GTM-P528B3":
			gtm = true
			if e.Method != "GET" || e.Status != 200 || e.ResourceType != "Script" {
				t.Errorf("GTM entry fields wrong: %+v", e)
			}
		case e.URL == "https://www.google-analytics.com/g/collect?tid=G-XXXX&en=page_view":
			gaCollect = true
			if e.Method != "POST" || e.Status != 204 || e.Initiator != "script" {
				t.Errorf("GA collect entry fields wrong: %+v", e)
			}
		}
	}
	if !gtm {
		t.Error("network log must include the googletagmanager gtm.js loader")
	}
	if !gaCollect {
		t.Error("network log must include the google-analytics /g/collect beacon")
	}
}

func TestFetchNetwork_NoLogHeaderReturnsNilSlice(t *testing.T) {
	// Older cache (or a fallback to a DOM-only renderer): no network header.
	// FetchNetwork must return a usable DOM + a nil slice, not an error.
	var seenRender string
	srv := networkCacheStub(t, "<html>dom only</html>", "", &seenRender)
	defer srv.Close()

	c := NewClient(WithCacheURL(srv.URL))
	resp, net, err := c.FetchNetwork(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("FetchNetwork must not error on a missing network header: %v", err)
	}
	if net != nil {
		t.Errorf("expected nil network slice when header absent, got %v", net)
	}
	if string(resp.Body) == "" {
		t.Error("DOM body should still be present without a network log")
	}
}

func TestParseNetworkLog(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int // -1 means nil
	}{
		{"empty string", "", -1},
		{"empty array", "[]", -1},
		{"malformed json", "{not json", -1},
		{"wrong shape (object)", `{"url":"x"}`, -1},
		{"one entry", `[{"url":"https://x/","method":"GET","status":200,"resource_type":"Document","initiator":"other"}]`, 1},
		{"two entries", `[{"url":"https://a/"},{"url":"https://b/"}]`, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseNetworkLog(c.raw)
			if c.want == -1 {
				if got != nil {
					t.Fatalf("parseNetworkLog(%q) = %v, want nil", c.raw, got)
				}
				return
			}
			if len(got) != c.want {
				t.Fatalf("parseNetworkLog(%q) len = %d, want %d", c.raw, len(got), c.want)
			}
		})
	}
}

func TestFetchNetwork_FallbackHasNoLog(t *testing.T) {
	// Cache unreachable → directFetch fallback → no network log, no error.
	c := NewClient(WithCacheURL("http://127.0.0.1:1")) // refused immediately
	resp, net, err := c.FetchNetwork(context.Background(), "https://example.com/")
	// The fallback path uses safehttp to fetch example.com directly. In CI
	// without network this may error; either way the network slice is nil
	// and we must not panic. Assert only the contract we control.
	if err == nil {
		if !resp.ViaFallback {
			t.Errorf("expected ViaFallback=true when cache refused, got %+v", resp)
		}
		if net != nil {
			t.Errorf("fallback path has no network log; got %v", net)
		}
	}
}
