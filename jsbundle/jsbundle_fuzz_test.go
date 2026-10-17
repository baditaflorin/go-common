package jsbundle

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzExtractScriptURLs parses attacker-controlled HTML. It must not panic,
// must not return duplicates, and must never leave a fragment on a URL.
func FuzzExtractScriptURLs(f *testing.F) {
	f.Add(`<script src="/a.js"></script><script src='b.js#hash'></script>`, "https://example.com/dir/page")
	f.Add(`<SCRIPT async SRC = "data:text/javascript,1"></SCRIPT>`, "https://example.com/")
	f.Add(`<script src="//cdn.example.net/x.js"></script>`, "http://[::1]:8080/")
	f.Fuzz(func(t *testing.T, html, rawBase string) {
		base, err := url.Parse(rawBase)
		if err != nil {
			return
		}
		out := ExtractScriptURLs(html, base)
		seen := make(map[string]bool, len(out))
		for _, u := range out {
			if strings.Contains(u, "#") {
				t.Fatalf("fragment left on %q", u)
			}
			if seen[u] {
				t.Fatalf("duplicate %q", u)
			}
			seen[u] = true
		}
	})
}

// FuzzFindMapURL parses the tail of an attacker-controlled bundle body.
func FuzzFindMapURL(f *testing.F) {
	f.Add("var a=1;\n//# sourceMappingURL=app.js.map\n", "https://example.com/static/app.js")
	f.Add("//@ sourceMappingURL=data:application/json;base64,e30=", "https://example.com/a.js")
	f.Add("//# sourceMappingURL= ../x.map  ", "https://example.com/a/b/c.js")
	f.Fuzz(func(t *testing.T, body, bundleURL string) {
		_ = findMapURL(body, bundleURL)
	})
}
