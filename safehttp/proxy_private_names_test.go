package safehttp

import (
	"net/http"
	"net/url"
	"testing"
)

var fakeProxy, _ = url.Parse("http://proxy.invalid:80")

func viaFakeProxy(*http.Request) (*url.URL, error) { return fakeProxy, nil }

// A compose container name that does not resolve from this container (stale or
// not started) must still go direct: the external proxy cannot resolve it
// either and would answer target_connect_resolve_failed for every request.
func TestProxySkippingPrivate_UnresolvableInternalNamesGoDirect(t *testing.T) {
	for _, raw := range []string{
		"http://go_stale_name-app-1:8270/health",
		"http://go-fleet-graph:8080/x",
		"http://db.internal/x",
		"http://svc.local:9000/",
	} {
		u, _ := url.Parse(raw)
		got, err := proxySkippingPrivateWith(&http.Request{URL: u}, viaFakeProxy)
		if err != nil || got != nil {
			t.Errorf("%s: proxy = %v, err = %v; want direct", raw, got, err)
		}
	}
}

func TestProxySkippingPrivate_PublicNameStillProxied(t *testing.T) {
	u, _ := url.Parse("https://public-name.example.com/")
	proxySkipCacheMu.Lock()
	proxySkipCache[u.Hostname()] = false
	proxySkipCacheMu.Unlock()
	got, err := proxySkippingPrivateWith(&http.Request{URL: u}, viaFakeProxy)
	if err != nil || got != fakeProxy {
		t.Fatalf("public name: proxy = %v, err = %v; want the configured proxy", got, err)
	}
}
