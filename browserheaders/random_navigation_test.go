package browserheaders_test

import (
	"net/http"
	"testing"

	"github.com/baditaflorin/go-common/browserheaders"
)

func TestApplyRandomReplacesStaleNavigationHeaders(t *testing.T) {
	h := http.Header{"Referer": {"https://old.example/"}, "Sec-Fetch-Site": {"same-origin"}}
	browserheaders.ApplyRandom(h)
	if h.Get("User-Agent") == "" || h.Get("Accept") == "" {
		t.Fatal("random persona did not provide browser headers")
	}
	if h.Get("Referer") != "" || h.Get("Sec-Fetch-Site") != "none" {
		t.Fatalf("stale navigation headers retained: %v", h)
	}
}

func TestNavigationMalformedURLsAreCrossSite(t *testing.T) {
	p := browserheaders.Personas()[0]
	for _, tc := range []struct{ target, referer string }{
		{"https://example.com/", "://bad"}, {"://bad", "https://example.com/"},
		{"relative/path", "https://example.com/"},
	} {
		h := http.Header{}
		p.ApplyNavigation(h, tc.target, tc.referer)
		if got := h.Get("Sec-Fetch-Site"); got != "cross-site" {
			t.Fatalf("target=%q referer=%q: got %q", tc.target, tc.referer, got)
		}
	}
}
