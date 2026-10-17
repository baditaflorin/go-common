package egressproxy

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// FuzzSplitAuthority parses the CONNECT target a browser (or anything else on
// the loopback port) sends. A target it accepts is later spliced into a request
// line for the upstream proxy, so it must not carry anything that could break
// out of that line.
func FuzzSplitAuthority(f *testing.F) {
	for _, s := range []string{
		"example.com:443", "[::1]:8080", "127.0.0.1:0", "a:65536", "a:1",
		":80", "host:", "host", "a b:80", "a\r\nHost: evil:80", "a@b:80",
		"a/b:80", "\x00:80", "[fe80::1%25eth0]:443", "例え.jp:443",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, hostport string) {
		host, port, err := splitAuthority(hostport)
		if err != nil {
			return
		}
		if host == "" {
			t.Fatalf("accepted empty host from %q", hostport)
		}
		if strings.ContainsAny(host, " \t\r\n\x00/\\@") {
			t.Fatalf("accepted host %q with a forbidden byte from %q", host, hostport)
		}
		if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
			t.Fatalf("accepted port %q from %q", port, hostport)
		}
		if strings.ContainsAny(net.JoinHostPort(host, port), "\r\n") {
			t.Fatalf("authority from %q contains a line break", hostport)
		}
	})
}

// FuzzParseUpstream: the upstream URL carries credentials. Whatever it is, the
// only error that may come back is the generic one (url.Parse errors quote the
// whole URL, password included), and an accepted URL must yield a usable
// host:port over plain http.
func FuzzParseUpstream(f *testing.F) {
	for _, s := range []string{
		"http://user:pass@proxy.example:8080", "http://proxy.example", "proxy.example:3128",
		"https://proxy.example", "socks5://p:1", "http://:80", "http://u:p@[::1]:9",
		"http://u:s3cr3t@host:notaport", "http://%zz", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		up, err := parseUpstream(raw)
		if err != nil {
			if err != errBadUpstream {
				t.Fatalf("parseUpstream returned a non-generic error %q for %q", err, raw)
			}
			return
		}
		if up.scheme != "http" {
			t.Fatalf("accepted scheme %q from %q", up.scheme, raw)
		}
		host, port, serr := net.SplitHostPort(up.addr)
		if serr != nil || host == "" || port == "" {
			t.Fatalf("unusable addr %q from %q", up.addr, raw)
		}
		if up.auth != "" && !strings.HasPrefix(up.auth, "Basic ") {
			t.Fatalf("unexpected auth form %q", up.auth)
		}
	})
}
