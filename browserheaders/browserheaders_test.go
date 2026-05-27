package browserheaders_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/browserheaders"
)

func TestRandom_NeverEmpty(t *testing.T) {
	for i := 0; i < 100; i++ {
		p := browserheaders.Random()
		if p.UserAgent == "" {
			t.Fatal("Random() returned empty UserAgent")
		}
		if p.Accept == "" {
			t.Fatal("Random() returned empty Accept")
		}
		if p.AcceptLanguage == "" {
			t.Fatal("Random() returned empty Accept-Language")
		}
		if p.AcceptEncoding == "" {
			t.Fatal("Random() returned empty Accept-Encoding")
		}
	}
}

func TestRandom_NoBotBranding(t *testing.T) {
	// The whole point: must never look like our fleet's ua.Build() output.
	for i := 0; i < 100; i++ {
		p := browserheaders.Random()
		ua := strings.ToLower(p.UserAgent)
		for _, bad := range []string{"+https://github.com", "/baditaflorin/", "go-search-duck-go", "search-duck-go"} {
			if strings.Contains(ua, strings.ToLower(bad)) {
				t.Fatalf("persona %q leaks bot branding %q", p.UserAgent, bad)
			}
		}
		if !strings.Contains(p.UserAgent, "Mozilla/5.0") {
			t.Fatalf("persona %q missing Mozilla/5.0 prefix (real browsers all have it)", p.UserAgent)
		}
	}
}

func TestApply_SetsBrowserHeaders(t *testing.T) {
	h := http.Header{}
	p := browserheaders.Random()
	p.Apply(h)

	for _, key := range []string{"User-Agent", "Accept", "Accept-Language", "Accept-Encoding"} {
		if h.Get(key) == "" {
			t.Errorf("Apply() did not set %s", key)
		}
	}
	// Sec-Fetch-* always set
	for _, key := range []string{"Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site", "Sec-Fetch-User"} {
		if h.Get(key) == "" {
			t.Errorf("Apply() did not set %s", key)
		}
	}
}

func TestApply_ChromeGetsSecChUa_FirefoxDoesNot(t *testing.T) {
	for _, p := range browserheaders.Personas() {
		h := http.Header{}
		p.Apply(h)
		isFirefoxOrSafari := strings.Contains(p.UserAgent, "Firefox/") || strings.Contains(p.UserAgent, "Version/")
		got := h.Get("Sec-Ch-Ua")
		if isFirefoxOrSafari && got != "" {
			t.Errorf("Firefox/Safari persona %q must not send Sec-Ch-Ua, got %q", p.UserAgent, got)
		}
		if !isFirefoxOrSafari && got == "" {
			t.Errorf("Chromium persona %q must send Sec-Ch-Ua", p.UserAgent)
		}
	}
}

func TestApply_AllPersonasIncludeBrotli(t *testing.T) {
	// Real modern browsers always include "br" in Accept-Encoding. Missing it
	// is a tell.
	for _, p := range browserheaders.Personas() {
		if !strings.Contains(p.AcceptEncoding, "br") {
			t.Errorf("persona %q missing br in Accept-Encoding: %q", p.UserAgent, p.AcceptEncoding)
		}
	}
}

func TestRandom_RotatesAcrossPool(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 200; i++ {
		seen[browserheaders.Random().UserAgent] = struct{}{}
	}
	if len(seen) < 5 {
		t.Fatalf("only %d distinct UAs in 200 draws — rotation broken", len(seen))
	}
}
