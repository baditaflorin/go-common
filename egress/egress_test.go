package egress

import (
	"net/http"
	"net/url"
	"testing"
)

func TestDirectOwnDestinations(t *testing.T) {
	for _, host := range []string{
		"fleet-graph.0exec.com", "pipe.0exec.com", "0exec.com", "FLEET-GRAPH.0EXEC.COM", "fleet-graph.0exec.com.",
		"js-proxy.0crawl.com", "openobserve.0own.com", "bastion.0docker.com", "pve01.0infer.com",
		"10.10.10.20", "172.17.0.5", "192.168.1.9", "127.0.0.1", "::1", "[::1]", "169.254.1.1",
		"100.92.211.122", "::ffff:10.0.0.1",
		"localhost", "host.docker.internal", "go-fleet-secrets", "go_foo-app-1",
	} {
		if !Direct(host) {
			t.Errorf("Direct(%q) = false, want direct", host)
		}
	}
}

func TestExternalDestinationsFollowTheProxy(t *testing.T) {
	for _, host := range []string{
		"example.com", "api.github.com", "evil0exec.com", "0exec.com.evil.net", "notfleet-0exec.com",
		"8.8.8.8", "1.1.1.1", "100.128.0.1", "172.32.0.1", "",
	} {
		if Direct(host) {
			t.Errorf("Direct(%q) = true, want the egress proxy", host)
		}
	}
}

func TestExtraDomainsAreAdditiveAndSafe(t *testing.T) {
	t.Setenv(ExtraDomainsEnv, " Corp.Example , *.partner.test, com, ,x")
	for _, host := range []string{"a.corp.example", "corp.example", "svc.partner.test"} {
		if !Direct(host) {
			t.Errorf("Direct(%q) = false with %s set", host, ExtraDomainsEnv)
		}
	}
	for _, host := range []string{"example.com", "other.test", "evilcorp.example"} {
		if Direct(host) {
			t.Errorf("Direct(%q) = true; a bare TLD or a lookalike must not become direct", host)
		}
	}
	if !Direct("fleet-graph.0exec.com") {
		t.Error("built-in fleet domains must stay direct when extras are set")
	}
}

// NO_PROXY is irrelevant here: the environment lookup is stubbed to always
// answer "use the proxy", so only our own decision can produce a direct
// connection.
func TestProxyIsOurDecisionNotNoProxys(t *testing.T) {
	sentinel, _ := url.Parse("http://egress.example.test:8080")
	prev := fromEnvironment
	fromEnvironment = func(*http.Request) (*url.URL, error) { return sentinel, nil }
	defer func() { fromEnvironment = prev }()

	for _, target := range []string{"https://fleet-graph.0exec.com/events", "https://pipe.0exec.com/x", "http://10.0.0.5/", "http://go-fleet-secrets:18140/"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if p, err := Proxy(req); err != nil || p != nil {
			t.Errorf("Proxy(%s) = %v, %v; want direct", target, p, err)
		}
	}
	for _, target := range []string{"https://example.com/", "https://evil0exec.com/", "http://8.8.8.8/"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if p, err := Proxy(req); err != nil || p != sentinel {
			t.Errorf("Proxy(%s) = %v, %v; want the environment proxy", target, p, err)
		}
	}
}

func TestTransportUsesProxyPolicy(t *testing.T) {
	tr := Transport()
	if tr.Proxy == nil {
		t.Fatal("Transport().Proxy is nil; want the egress policy")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://fleet-graph.0exec.com/", nil)
	if p, err := tr.Proxy(req); err != nil || p != nil {
		t.Fatalf("Transport proxy for a fleet host = %v, %v; want direct", p, err)
	}
	if Transport() == tr {
		t.Fatal("Transport() must return a fresh transport on every call")
	}
}
