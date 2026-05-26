package proxysupplier_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/proxysupplier"
)

func TestNoneSupplier(t *testing.T) {
	s := proxysupplier.NewFromConfig(proxysupplier.Config{Supplier: ""})
	if s.Name() != "none" {
		t.Fatalf("want none, got %q", s.Name())
	}
	if s.ProxyURL() != "" {
		t.Fatalf("want empty ProxyURL, got %q", s.ProxyURL())
	}
	if proxysupplier.HTTPClient(s, time.Second) != nil {
		t.Fatal("HTTPClient should return nil for none supplier")
	}
}

func TestPlainProxiesSupplier(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier: "plain_proxies",
		Host:     "proxy.example.com",
		Port:     "1338",
		Username: "user",
		Password: "pass",
	}
	s := proxysupplier.NewFromConfig(cfg)
	if s.Name() != "plain_proxies" {
		t.Fatalf("want plain_proxies, got %q", s.Name())
	}
	want := "http://user:pass@proxy.example.com:1338/"
	if s.ProxyURL() != want {
		t.Fatalf("ProxyURL: want %q, got %q", want, s.ProxyURL())
	}
}

func TestEnvSupplierExternalURL(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:         "env",
		ExternalProxyURL: "http://rotate:secret@p.example.io:80",
	}
	s := proxysupplier.NewFromConfig(cfg)
	if s.Name() != "env" {
		t.Fatalf("want env, got %q", s.Name())
	}
	if s.ProxyURL() != "http://rotate:secret@p.example.io:80" {
		t.Fatalf("unexpected ProxyURL: %q", s.ProxyURL())
	}
}

func TestEnvSupplierFallbackToHostPort(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier: "env",
		Host:     "proxy.example.com",
		Port:     "8080",
	}
	s := proxysupplier.NewFromConfig(cfg)
	want := "http://proxy.example.com:8080/"
	if s.ProxyURL() != want {
		t.Fatalf("want %q, got %q", want, s.ProxyURL())
	}
}

func TestSelfProxyGuard_Loopback(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "0.0.0.0"} {
		cfg := proxysupplier.Config{
			Supplier: "plain_proxies",
			Host:     host,
			Port:     "3001",
		}
		s := proxysupplier.NewFromConfig(cfg)
		if s.Name() != "none" {
			t.Errorf("loopback host %q should trigger self-proxy guard, got supplier %q", host, s.Name())
		}
	}
}

func TestEmptyHostReturnsNone(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier: "plain_proxies",
		Host:     "",
	}
	s := proxysupplier.NewFromConfig(cfg)
	if s.Name() != "none" {
		t.Fatalf("empty host: want none, got %q", s.Name())
	}
}

func TestHTTPClient_NonNilForValidSupplier(t *testing.T) {
	// Can't use a loopback address as proxy URL — the self-proxy guard
	// correctly blocks it.  Instead verify that a non-loopback URL produces
	// a non-nil client with the right transport proxy set.
	cfg := proxysupplier.Config{
		Supplier:         "env",
		ExternalProxyURL: "http://proxy.example.com:8080/",
	}
	s := proxysupplier.NewFromConfig(cfg)
	client := proxysupplier.HTTPClient(s, 2*time.Second)
	if client == nil {
		t.Fatal("expected non-nil *http.Client for a valid proxy URL")
	}
	// Verify the transport has a proxy function configured.
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("Transport.Proxy should be set")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	proxyURL, err := tr.Proxy(req)
	if err != nil || proxyURL == nil {
		t.Fatalf("Transport.Proxy returned unexpected result: url=%v err=%v", proxyURL, err)
	}
	if proxyURL.Host != "proxy.example.com:8080" {
		t.Fatalf("unexpected proxy host: %q", proxyURL.Host)
	}
}

func TestUnknownSupplierReturnsNone(t *testing.T) {
	s := proxysupplier.NewFromConfig(proxysupplier.Config{Supplier: "webshare_v2"})
	if s.Name() != "none" {
		t.Fatalf("unknown supplier should return none, got %q", s.Name())
	}
}

// --- multi supplier tests ---------------------------------------------------

func TestMultiSupplier_SingleURL(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:  "multi",
		ProxyURLs: "http://user:pass@proxy1.example.com:1338",
	}
	s := proxysupplier.NewFromConfig(cfg)
	if s.Name() != "multi" {
		t.Fatalf("want multi, got %q", s.Name())
	}
	got := s.ProxyURL()
	if got != "http://user:pass@proxy1.example.com:1338" {
		t.Fatalf("unexpected ProxyURL: %q", got)
	}
}

func TestMultiSupplier_WeightedDistribution(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:     "multi",
		ProxyURLs:    "http://user:pass@proxy1.example.com:1338,http://user:pass@proxy2.example.com:80",
		ProxyWeights: "90,10",
	}
	s := proxysupplier.NewFromConfig(cfg)
	if s.Name() != "multi" {
		t.Fatalf("want multi, got %q", s.Name())
	}

	counts := map[string]int{}
	const n = 10000
	for i := 0; i < n; i++ {
		counts[s.ProxyURL()]++
	}
	p1 := float64(counts["http://user:pass@proxy1.example.com:1338"]) / n
	// With weights 90:10 we expect proxy1 ~90% of the time.
	// Allow ±5% tolerance to avoid flaky tests.
	if p1 < 0.85 || p1 > 0.95 {
		t.Fatalf("proxy1 got %.1f%% of requests, want ~90%%", p1*100)
	}
}

func TestMultiSupplier_EqualWeightDefault(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:  "multi",
		ProxyURLs: "http://a.example.com:80,http://b.example.com:80",
		// no ProxyWeights — should default to equal
	}
	s := proxysupplier.NewFromConfig(cfg)
	counts := map[string]int{}
	const n = 10000
	for i := 0; i < n; i++ {
		counts[s.ProxyURL()]++
	}
	pA := float64(counts["http://a.example.com:80"]) / n
	if pA < 0.45 || pA > 0.55 {
		t.Fatalf("equal-weight: proxy A got %.1f%%, want ~50%%", pA*100)
	}
}

func TestMultiSupplier_EmptyURLsReturnsNone(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:  "multi",
		ProxyURLs: "",
	}
	s := proxysupplier.NewFromConfig(cfg)
	if s.Name() != "none" {
		t.Fatalf("empty PROXY_URLS: want none, got %q", s.Name())
	}
}

func TestMultiSupplier_LoopbackDropped(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:  "multi",
		ProxyURLs: "http://127.0.0.1:8080,http://proxy.example.com:80",
		// loopback entry should be silently dropped
	}
	s := proxysupplier.NewFromConfig(cfg)
	if s.Name() != "multi" {
		t.Fatalf("want multi (one live entry remains), got %q", s.Name())
	}
	for i := 0; i < 100; i++ {
		if got := s.ProxyURL(); got != "http://proxy.example.com:80" {
			t.Fatalf("loopback not dropped: got %q", got)
		}
	}
}

func TestMultiSupplier_HTTPClient_PerRequestProxy(t *testing.T) {
	cfg := proxysupplier.Config{
		Supplier:     "multi",
		ProxyURLs:    "http://proxy1.example.com:80,http://proxy2.example.com:80",
		ProxyWeights: "50,50",
	}
	s := proxysupplier.NewFromConfig(cfg)
	client := proxysupplier.HTTPClient(s, 2*time.Second)
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("Transport.Proxy should be set")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil {
		t.Fatalf("Proxy() returned url=%v err=%v", u, err)
	}
}
