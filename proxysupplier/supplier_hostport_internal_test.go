package proxysupplier

import (
	"encoding/json"
	"fmt"
	"go/version"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"testing"
	"time"
)

// TestBuildURL pins the proxy URL buildURL produces. A host that is an IPv6
// literal must be bracketed: "http://::1:3001/" is not a URL, and Go 1.26's
// net/url (GODEBUG urlstrictcolons, on by default for a module whose go line is
// 1.26 or later) refuses to parse it. A name and an IPv4 host are unchanged.
func TestBuildURL(t *testing.T) {
	tests := []struct {
		name                          string
		protocol, host, port, user, p string
		want                          string
		wantHost                      string // u.Hostname() after parsing want
	}{
		{"name", "http", "proxy.example.com", "8080", "", "", "http://proxy.example.com:8080/", "proxy.example.com"},
		{"ipv4", "http", "10.0.0.5", "3128", "", "", "http://10.0.0.5:3128/", "10.0.0.5"},
		{"ipv6 loopback", "http", "::1", "3001", "", "", "http://[::1]:3001/", "::1"},
		{"ipv6 global", "http", "2001:db8::1", "8080", "", "", "http://[2001:db8::1]:8080/", "2001:db8::1"},
		{"ipv6 already bracketed", "http", "[::1]", "3001", "", "", "http://[::1]:3001/", "::1"},
		{"ipv6 other scheme", "socks5", "::1", "1080", "", "", "socks5://[::1]:1080/", "::1"},
		{"default protocol", "", "::1", "3001", "", "", "http://[::1]:3001/", "::1"},

		{"name with credentials", "http", "proxy.example.com", "8080", "user", "pass", "http://user:pass@proxy.example.com:8080/", "proxy.example.com"},
		{"ipv4 with credentials", "http", "10.0.0.5", "3128", "user", "pass", "http://user:pass@10.0.0.5:3128/", "10.0.0.5"},
		{"ipv6 with credentials", "http", "::1", "3001", "user", "pass", "http://user:pass@[::1]:3001/", "::1"},
		{"ipv6 global with credentials", "http", "2001:db8::1", "8080", "user", "pass", "http://user:pass@[2001:db8::1]:8080/", "2001:db8::1"},
		// One of the two credentials is not enough: buildURL falls back to the form without any.
		{"ipv6 username only", "http", "::1", "3001", "user", "", "http://[::1]:3001/", "::1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildURL(tc.protocol, tc.host, tc.port, tc.user, tc.p)
			if got != tc.want {
				t.Fatalf("buildURL(%q, %q, %q, %q, %q) = %q, want %q", tc.protocol, tc.host, tc.port, tc.user, tc.p, got, tc.want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", got, err)
			}
			if u.Hostname() != tc.wantHost || u.Port() != tc.port {
				t.Errorf("url.Parse(%q) gives host %q port %q, want host %q port %q", got, u.Hostname(), u.Port(), tc.wantHost, tc.port)
			}
		})
	}

	if got := buildURL("http", "", "3001", "user", "pass"); got != "" {
		t.Errorf("buildURL with an empty host = %q, want \"\"", got)
	}
}

// TestIsSelfProxy_LoopbackUnderStrictColons is the regression test for the
// fail-open: with a strict net/url, buildURL used to give an unparseable URL for
// the host "::1", isSelfProxy treated the parse error as "not a self proxy", and
// the supplier that routes back to this process was kept.
//
// GODEBUG is read again by net/url when it changes at run time, so t.Setenv is
// enough to switch the strict parser on for this test. The setting exists from
// Go 1.26; on an older toolchain it is ignored and the strict parser cannot be
// forced, so the test skips there (TestBuildURL covers the string on every Go).
func TestIsSelfProxy_LoopbackUnderStrictColons(t *testing.T) {
	t.Setenv("GODEBUG", "urlstrictcolons=1")
	// The form buildURL used to write. Built with Sprintf because staticcheck
	// (SA1007) rejects the literal as an invalid URL, which is the point.
	unbracketed := fmt.Sprintf("http://%s:%s/", "::1", "3001")
	if _, err := url.Parse(unbracketed); err == nil {
		if version.Compare(runtime.Version(), "go1.26") >= 0 {
			t.Fatal("GODEBUG=urlstrictcolons=1 did not make net/url reject an unbracketed IPv6 host; this test would pass without testing anything")
		}
		t.Skipf("%s has no urlstrictcolons setting, so net/url cannot be made strict", runtime.Version())
	}

	if !isSelfProxy(buildURL("http", "::1", "3001", "", "")) {
		t.Error(`isSelfProxy(buildURL("http", "::1", "3001", "", "")) = false, want true`)
	}
	if !isSelfProxy(buildURL("http", "::1", "3001", "user", "pass")) {
		t.Error(`isSelfProxy(buildURL("http", "::1", "3001", "user", "pass")) = false, want true`)
	}
	// The whole path a service takes: the guard drops the supplier.
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "0.0.0.0"} {
		s := NewFromConfig(Config{Supplier: "plain_proxies", Host: host, Port: "3001"})
		if s.Name() != "none" {
			t.Errorf("loopback host %q should trigger the self-proxy guard under a strict net/url, got supplier %q", host, s.Name())
		}
	}
	// The guard must not start refusing an IPv6 proxy that is somewhere else.
	if isSelfProxy(buildURL("http", "2001:db8::1", "3001", "", "")) {
		t.Error("isSelfProxy treated the documentation address 2001:db8::1 as this process")
	}
}

// TestWebshareDirect_IPv6EntryIsBracketed: an entry whose proxy_address is an
// IPv6 literal must give a URL that parses and an address key in the form the
// dialed connection reports, "[addr]:port", which is what MarkResult is keyed by.
func TestWebshareDirect_IPv6EntryIsBracketed(t *testing.T) {
	entries := []webshareEntry{{Username: "u", Password: "p", ProxyAddress: "2001:db8::1", Port: 5000, Valid: true}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(webshareListResponse{Count: len(entries), Results: entries})
	}))
	defer srv.Close()
	withWebshareDirectBaseURL(t, srv.URL)

	s := newTestSupplier(t)
	got := waitForNonEmptyProxyURL(t, s, time.Second)

	if want := "http://u:p@[2001:db8::1]:5000"; got != want {
		t.Fatalf("ProxyURL() = %q, want %q", got, want)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", got, err)
	}
	if u.Hostname() != "2001:db8::1" || u.Port() != "5000" {
		t.Errorf("parsed host %q port %q, want 2001:db8::1 and 5000", u.Hostname(), u.Port())
	}

	// The address a caller recovers from conn.RemoteAddr() is net.JoinHostPort(...);
	// reporting it must reach the entry.
	addr := net.JoinHostPort("2001:db8::1", "5000")
	if AddrFromProxyURL(got) != addr {
		t.Errorf("AddrFromProxyURL(%q) = %q, want %q", got, AddrFromProxyURL(got), addr)
	}
	s.(ResultReporter).MarkResult(addr, false)
	if u := s.ProxyURL(); u != "" {
		t.Errorf("ProxyURL() = %q after MarkResult(%q, false) on the only entry, want \"\" (the failure did not reach the entry)", u, addr)
	}
}
