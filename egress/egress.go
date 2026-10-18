// Package egress decides whether an outbound request goes through the
// environment's egress proxy (HTTP_PROXY / HTTPS_PROXY / NO_PROXY).
//
// The rule is by destination, not by host configuration: traffic to the
// fleet's own services is always direct, even when the service is addressed by
// its public name (pipe.0exec.com, fleet-graph.0exec.com), because we own it
// and have nothing to hide from it. The proxy is for third-party targets: a
// destination that is neither one of our domains, nor a private, loopback or
// tailnet address, nor a bare container name. Those follow the standard
// environment behaviour (http.ProxyFromEnvironment, so NO_PROXY still applies).
//
// Use it for the clients that talk to our own services (the fetch cache, the
// rate coordinator, the Graph collector). server.New also applies it to
// http.DefaultTransport (InstallDefaultTransport), so a plain http.Client, which
// is what a service uses for calls to its siblings and for its dependency
// probes, follows the same rule without any code of its own. It is deliberately
// NOT applied to safehttp: that is the generic public-web client, and the runner's
// strict egress policy (go_fleet_runner #428) keeps even our own public hostnames
// on the proxy for proxy_egress services that use it to scan targets.
package egress

import (
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
)

// fleetDomains are the apex domains the fleet owns. A host is ours when it is
// one of them or any subdomain of one.
var fleetDomains = []string{"0exec.com", "0crawl.com", "0own.com", "0docker.com", "0infer.com"}

// ExtraDomainsEnv adds apex domains to the built-in list without a release:
// a comma-separated list such as "example.org,corp.example". Entries need at
// least one dot, so a bare TLD cannot be made direct by accident.
const ExtraDomainsEnv = "FLEET_DIRECT_DOMAINS"

// tailnet is the Tailscale CGNAT range; netip's IsPrivate does not cover it.
var tailnet = netip.MustParsePrefix("100.64.0.0/10")

// Direct reports whether a destination host (no port) is ours and must be
// reached without the egress proxy.
func Direct(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "" {
		return false
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		ip = ip.Unmap()
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || tailnet.Contains(ip)
	}
	if h == "localhost" || h == "host.docker.internal" {
		return true
	}
	// A bare name (go-fleet-secrets, a compose service) only resolves on the
	// local network; an external proxy cannot resolve it.
	if !strings.Contains(h, ".") {
		return true
	}
	return fleetHost(h)
}

func fleetHost(h string) bool {
	for _, d := range fleetDomains {
		if sameOrSubdomain(h, d) {
			return true
		}
	}
	for _, d := range extraDomains() {
		if sameOrSubdomain(h, d) {
			return true
		}
	}
	return false
}

// sameOrSubdomain matches on a label boundary, so "evil0exec.com" and
// "0exec.com.evil.net" are not "0exec.com".
func sameOrSubdomain(h, domain string) bool {
	return h == domain || strings.HasSuffix(h, "."+domain)
}

func extraDomains() []string {
	raw := os.Getenv(ExtraDomainsEnv)
	if raw == "" {
		return nil
	}
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		d := strings.ToLower(strings.TrimSpace(entry))
		d = strings.TrimPrefix(strings.TrimPrefix(d, "*"), ".")
		d = strings.TrimSuffix(d, ".")
		if strings.Contains(d, ".") {
			out = append(out, d)
		}
	}
	return out
}

// fromEnvironment is the standard environment proxy lookup; a variable so the
// tests are independent of http.ProxyFromEnvironment's process-wide cache.
var fromEnvironment = http.ProxyFromEnvironment

// Proxy is an http.Transport.Proxy function: no proxy for our own
// destinations, the environment's proxy for everything else.
func Proxy(req *http.Request) (*url.URL, error) {
	if req.URL != nil && Direct(req.URL.Hostname()) {
		return nil, nil
	}
	return fromEnvironment(req)
}

// Transport returns a clone of http.DefaultTransport whose proxy policy is
// Proxy. Each call returns a fresh transport; callers own its lifetime. Prefer it
// to relying on InstallDefaultTransport for a client that must keep the rule
// whatever the rest of the program does to the default transport.
func Transport() *http.Transport {
	t := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t = base.Clone()
	}
	t.Proxy = Proxy
	return t
}
