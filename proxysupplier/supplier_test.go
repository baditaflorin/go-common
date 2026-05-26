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
