package proxysupplier_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/proxysupplier"
)

func TestNoProxy_BypassedHostsReturnNil(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:         "env",
		ExternalProxyURL: "http://upstream-proxy.example.com:8080",
		NoProxy:          ".0crawl.com,.0exec.com,localhost,127.0.0.1,host.docker.internal,10.0.0.0/8,172.16.0.0/12",
	}
	s := proxysupplier.NewFromConfig(cfg)
	if s.Name() != "env" {
		t.Fatalf("want env, got %q", s.Name())
	}
	client := proxysupplier.HTTPClient(s, 2*time.Second)
	tr := client.Transport.(*http.Transport)

	check := func(url string, wantProxy bool) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		got, _ := tr.Proxy(req)
		if wantProxy && got == nil {
			t.Errorf("%s: expected proxy, got direct", url)
		}
		if !wantProxy && got != nil {
			t.Errorf("%s: expected direct (bypass), got proxy %s", url, got)
		}
	}

	// These MUST bypass the proxy.
	check("https://phone-extractor.0crawl.com/", false)
	check("https://search-duck-go.0exec.com/", false)
	check("http://host.docker.internal:18021/verify", false)
	check("http://localhost:8080/health", false)
	check("http://127.0.0.1:9090/", false)
	check("http://10.10.10.30:23481/", false)   // 10.0.0.0/8 → matches
	check("http://172.20.0.5:3401/?q=test", false) // 172.16.0.0/12 → matches docker bridge

	// These MUST go through the proxy.
	check("https://html.duckduckgo.com/html/?q=test", true)
	check("https://www.example.com/", true)
	check("https://api.example.org/", true)
}

func TestNoProxy_WildcardBypassesEverything(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:         "env",
		ExternalProxyURL: "http://upstream-proxy.example.com:8080",
		NoProxy:          "*",
	}
	s := proxysupplier.NewFromConfig(cfg)
	client := proxysupplier.HTTPClient(s, 2*time.Second)
	tr := client.Transport.(*http.Transport)
	req, _ := http.NewRequest(http.MethodGet, "https://duckduckgo.com/", nil)
	got, _ := tr.Proxy(req)
	if got != nil {
		t.Fatalf("wildcard NO_PROXY: expected direct, got proxy %s", got)
	}
}

func TestNoProxy_EmptyMeansEverythingProxied(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:         "env",
		ExternalProxyURL: "http://upstream-proxy.example.com:8080",
		// NoProxy left empty
	}
	s := proxysupplier.NewFromConfig(cfg)
	client := proxysupplier.HTTPClient(s, 2*time.Second)
	tr := client.Transport.(*http.Transport)
	req, _ := http.NewRequest(http.MethodGet, "http://localhost:8080/", nil)
	got, _ := tr.Proxy(req)
	if got == nil {
		t.Fatal("empty NO_PROXY: expected proxy, got direct")
	}
}

func TestNoProxy_AppliesToMultiSupplier(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:     "multi",
		ProxyURLs:    "http://a.example.com:80,http://b.example.com:80",
		ProxyWeights: "50,50",
		NoProxy:      ".0crawl.com",
	}
	s := proxysupplier.NewFromConfig(cfg)
	if s.Name() != "multi" {
		t.Fatalf("want multi, got %q", s.Name())
	}
	client := proxysupplier.HTTPClient(s, 2*time.Second)
	tr := client.Transport.(*http.Transport)

	req, _ := http.NewRequest(http.MethodGet, "https://phone-extractor.0crawl.com/", nil)
	got, _ := tr.Proxy(req)
	if got != nil {
		t.Errorf(".0crawl.com via multi-supplier should bypass, got %s", got)
	}

	req, _ = http.NewRequest(http.MethodGet, "https://duckduckgo.com/", nil)
	got, _ = tr.Proxy(req)
	if got == nil {
		t.Error("duckduckgo.com via multi-supplier should USE proxy, got direct")
	}
}

func TestNoProxy_CaseInsensitive(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:         "env",
		ExternalProxyURL: "http://p.example.com:80",
		NoProxy:          ".0CRAWL.com,LOCALHOST",
	}
	s := proxysupplier.NewFromConfig(cfg)
	client := proxysupplier.HTTPClient(s, 2*time.Second)
	tr := client.Transport.(*http.Transport)

	// Hostname-with-uppercase should still bypass.
	req, _ := http.NewRequest(http.MethodGet, "https://Phone-Extractor.0crawl.com/", nil)
	got, _ := tr.Proxy(req)
	if got != nil {
		t.Errorf("expected bypass for uppercase domain, got %s", got)
	}
}

func TestNoProxy_LowercaseEnvVarFallback(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", ".0crawl.com")
	t.Setenv("PROXY_SUPPLIER", "env")
	t.Setenv("EXTERNAL_PROXY_URL", "http://p.example.com:80")
	s := proxysupplier.New()
	client := proxysupplier.HTTPClient(s, 2*time.Second)
	tr := client.Transport.(*http.Transport)
	req, _ := http.NewRequest(http.MethodGet, "https://x.0crawl.com/", nil)
	got, _ := tr.Proxy(req)
	if got != nil {
		t.Errorf("lowercase no_proxy env should be honored; got proxy %s", got)
	}
}
