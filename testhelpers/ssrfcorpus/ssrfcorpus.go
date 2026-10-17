// Package ssrfcorpus is one shared table of server-side-request-forgery probe
// targets, for testing any code that decides whether a host may be contacted:
// safehttp's guard, egressproxy's guard, a fetcher's default client.
//
// Every case is decidable without the network. Literal IPs are judged on the
// address alone; the few that go through the resolver (localhost, the
// non-canonical spellings of 127.0.0.1) either resolve to a blocked address or
// fail to resolve, and either way the target must be refused.
//
//	for _, c := range ssrfcorpus.Cases {
//	    err := guard(ctx, c.Host)
//	    if c.Blocked && err == nil { t.Errorf("%s: allowed", c.Name) }
//	}
//
// Add a case here when a bypass is found anywhere, so every guard is tested
// against it.
package ssrfcorpus

import (
	"net"
	"strings"
)

// Case is one probe target.
type Case struct {
	// Name identifies the case in test output.
	Name string
	// Host is the host as url.URL.Hostname() returns it: no brackets, no port.
	Host string
	// Blocked is true when a guard must refuse the target. False cases are
	// public addresses next to a blocked range, to catch a guard that
	// over-blocks.
	Blocked bool
	// NeedsResolver marks hosts a guard can only judge by resolving them, so a
	// test should give them a short timeout.
	NeedsResolver bool
	// Gap, when set, marks a target a guard does NOT refuse today. It documents
	// a known hole: tests log the verdict instead of asserting it, and say when
	// the hole has closed.
	Gap string
}

// URL returns an http URL for the case, with IPv6 hosts in brackets.
func (c Case) URL() string {
	host := c.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "http://" + host + "/"
}

// IsLiteralIP reports whether Host is an IP literal in canonical form.
func (c Case) IsLiteralIP() bool { return net.ParseIP(c.Host) != nil }

func blocked(name, host string) Case  { return Case{Name: name, Host: host, Blocked: true} }
func allowed(name, host string) Case  { return Case{Name: name, Host: host} }
func gap(name, host, why string) Case { return Case{Name: name, Host: host, Gap: why} }
func viaResolver(name, host string) Case {
	return Case{Name: name, Host: host, Blocked: true, NeedsResolver: true}
}

// Cases is the corpus.
var Cases = []Case{
	// Loopback.
	blocked("loopback-v4", "127.0.0.1"),
	blocked("loopback-v4-top", "127.255.255.254"),
	blocked("loopback-v6", "::1"),
	// IPv4 inside IPv6: the classic way past a guard that only checks v4.
	blocked("v4mapped-loopback", "::ffff:127.0.0.1"),
	blocked("v4mapped-loopback-hex", "::ffff:7f00:1"),
	blocked("v4mapped-cloud-metadata", "::ffff:169.254.169.254"),
	blocked("v4mapped-private", "::ffff:10.0.0.1"),
	// RFC 1918.
	blocked("private-10", "10.0.0.1"),
	blocked("private-10-top", "10.255.255.255"),
	blocked("private-172-low", "172.16.0.1"),
	blocked("private-172-high", "172.31.255.255"),
	blocked("private-192-168", "192.168.1.1"),
	// Link-local, including the cloud metadata endpoint.
	blocked("cloud-metadata", "169.254.169.254"),
	blocked("link-local-v6", "fe80::1"),
	// CGNAT and unique-local.
	blocked("cgnat-low", "100.64.0.1"),
	blocked("cgnat-high", "100.127.255.255"),
	blocked("ula-fd", "fd00::1"),
	blocked("ula-fc", "fc00::1"),
	// Unspecified, "this network", multicast.
	blocked("unspecified-v4", "0.0.0.0"),
	blocked("unspecified-v6", "::"),
	blocked("this-network", "0.1.2.3"),
	blocked("multicast-v4", "224.0.0.1"),
	blocked("multicast-v6", "ff02::1"),
	// Names and spellings that reach loopback only through the resolver.
	viaResolver("localhost", "localhost"),
	viaResolver("localhost-upper", "LOCALHOST"),
	viaResolver("localhost-trailing-dot", "localhost."),
	viaResolver("decimal-ip", "2130706433"),
	viaResolver("hex-ip", "0x7f000001"),
	viaResolver("dotted-hex-ip", "0x7f.0.0.1"),
	viaResolver("dotted-octal-ip", "0177.0.0.1"),
	viaResolver("short-ip", "127.1"),
	// A missing host is never a target.
	blocked("empty-host", ""),

	// Public addresses, including the ones just outside each blocked range.
	allowed("public-dns-v4", "8.8.8.8"),
	allowed("public-dns-v4-alt", "1.1.1.1"),
	allowed("public-web", "93.184.216.34"),
	allowed("public-v6", "2001:4860:4860::8888"),
	allowed("v4mapped-public", "::ffff:8.8.8.8"),
	allowed("just-below-10", "9.255.255.255"),
	allowed("just-above-10", "11.0.0.1"),
	allowed("just-below-172-16", "172.15.255.255"),
	allowed("just-above-172-31", "172.32.0.1"),
	allowed("just-below-cgnat", "100.63.255.255"),
	allowed("just-above-cgnat", "100.128.0.1"),
	allowed("just-below-link-local", "169.253.255.255"),
	allowed("just-above-link-local", "169.255.0.1"),
	allowed("just-below-192-168", "192.167.255.255"),
	allowed("just-above-192-168", "192.169.0.1"),

	// Known holes. Reaching 127.0.0.1 this way needs a NAT64 or 6to4 gateway
	// between the caller and the target, which a typical container host does not
	// have; they are listed so that a guard that closes them can say so here.
	gap("nat64-loopback", "64:ff9b::7f00:1", "NAT64 well-known prefix embedding 127.0.0.1"),
	gap("nat64-cloud-metadata", "64:ff9b::a9fe:a9fe", "NAT64 well-known prefix embedding 169.254.169.254"),
	gap("nat64-private", "64:ff9b::a00:1", "NAT64 well-known prefix embedding 10.0.0.1"),
	gap("6to4-loopback", "2002:7f00:1::", "6to4 prefix embedding 127.0.0.1"),
	gap("6to4-private", "2002:c0a8:101::", "6to4 prefix embedding 192.168.1.1"),
	allowed("nat64-public", "64:ff9b::808:808"),
	allowed("6to4-public", "2002:808:808::"),
}
