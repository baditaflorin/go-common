package safehttp

import (
	"net/http"
	"net/url"
	"testing"
)

// Our own destinations never use the egress proxy, whatever NO_PROXY says and
// without a DNS lookup (the names below do not resolve anywhere); a third-party
// host still follows the environment proxy. The environment lookup is stubbed
// to always answer "use the proxy", so only our own decision can go direct.
func TestProxySkippingPrivateSendsFleetHostsDirect(t *testing.T) {
	sentinel, _ := url.Parse("http://egress.example.test:8080")
	prev := envProxy
	envProxy = func(*http.Request) (*url.URL, error) { return sentinel, nil }
	defer func() { envProxy = prev }()

	for _, target := range []string{
		"https://fleet-graph.0exec.com/events",
		"https://pipe.0exec.com/x",
		"https://domain.0crawl.com/",
		"http://go-fleet-secrets:18140/secrets/x",
		"http://100.92.211.122:18241/metrics",
	} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if proxy, err := proxySkippingPrivate(req); err != nil || proxy != nil {
			t.Errorf("proxySkippingPrivate(%s) = %v, %v; want direct", target, proxy, err)
		}
	}
	// .invalid never resolves (RFC 6761), so this reaches the environment lookup.
	req, _ := http.NewRequest(http.MethodGet, "https://third-party.example.invalid/", nil)
	if proxy, err := proxySkippingPrivate(req); err != nil || proxy != sentinel {
		t.Errorf("third-party host = %v, %v; want the environment proxy", proxy, err)
	}
}
