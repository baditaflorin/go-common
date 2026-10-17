package safehttp_test

import (
	"net"
	"net/netip"
	"testing"

	"github.com/baditaflorin/go-common/safehttp"
)

// FuzzNormalizeURL: operator- and attacker-supplied URLs go through here
// before any guard. A URL that normalises without error must be http(s) with
// a host, otherwise the guard downstream is looking at the wrong thing.
func FuzzNormalizeURL(f *testing.F) {
	for _, s := range []string{
		"example.com", "http://example.com/a?b#c", "  https://EXAMPLE.com:8443  ",
		"//evil.example", "ftp://example.com", "http://", "http://[::1]:80/",
		"http://user:pass@127.0.0.1/", "http://0x7f.1/", "javascript:alert(1)",
		"http://a b/", "http://%00/", "https://例え.jp/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := safehttp.NormalizeURL(raw)
		if err != nil {
			return
		}
		if u == nil {
			t.Fatalf("NormalizeURL(%q) returned nil URL and nil error", raw)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("NormalizeURL(%q) accepted scheme %q", raw, u.Scheme)
		}
		if u.Host == "" {
			t.Fatalf("NormalizeURL(%q) accepted an empty host", raw)
		}
	})
}

// documentedBlocked lists the ranges safehttp.IsBlocked documents. It is a
// second, independent statement of the policy (prefix tests on netip), so a
// disagreement means one of the two is wrong, most likely an IPv4-in-IPv6
// form slipping past the guard.
var documentedBlocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// FuzzIsBlocked checks every IPv4 and IPv6 byte pattern, including IPv4-mapped
// IPv6 (::ffff:a.b.c.d), against the documented blocklist.
func FuzzIsBlocked(f *testing.F) {
	f.Add([]byte{127, 0, 0, 1})
	f.Add([]byte{10, 1, 2, 3})
	f.Add([]byte{100, 64, 0, 1})
	f.Add([]byte{8, 8, 8, 8})
	f.Add([]byte(net.ParseIP("::ffff:127.0.0.1")))
	f.Add([]byte(net.ParseIP("::ffff:169.254.169.254")))
	f.Add([]byte(net.ParseIP("fd00::1")))
	f.Add([]byte(net.ParseIP("2001:4860:4860::8888")))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) != 4 && len(b) != 16 {
			return
		}
		safehttp.SetAllowedPrivateIPs(nil) // never let an env allowlist mask a finding
		addr, ok := netip.AddrFromSlice(b)
		if !ok {
			return
		}
		addr = addr.Unmap()
		for _, p := range documentedBlocked {
			if p.Contains(addr) {
				if !safehttp.IsBlocked(net.IP(b)) {
					t.Fatalf("IsBlocked(%v) = false, but %v is inside documented range %v", net.IP(b), addr, p)
				}
				return
			}
		}
	})
}
