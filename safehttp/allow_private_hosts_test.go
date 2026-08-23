package safehttp_test

import (
	"net"
	"testing"

	"github.com/baditaflorin/go-common/safehttp"
)

// resetPrivateAllowlists clears both allowlists before and after a test so
// mutations via SetAllowedPrivateIPs/SetAllowedPrivateHosts never leak into
// sibling tests (these are process-global, mirroring the env-var-backed
// production path).
func resetPrivateAllowlists(t *testing.T) {
	t.Helper()
	safehttp.SetAllowedPrivateIPs(nil)
	safehttp.SetAllowedPrivateHosts(nil)
	t.Cleanup(func() {
		safehttp.SetAllowedPrivateIPs(nil)
		safehttp.SetAllowedPrivateHosts(nil)
	})
}

func mustParseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("invalid test IP: %s", s)
	}
	return ip
}

// TestIsBlockedForHostDefaultMatchesIsBlocked proves that with neither
// allowlist configured (the state of every fleet service today),
// IsBlockedForHost is a pure passthrough to IsBlocked — this feature is
// opt-in and changes nothing for existing callers.
func TestIsBlockedForHostDefaultMatchesIsBlocked(t *testing.T) {
	resetPrivateAllowlists(t)
	cases := []string{"127.0.0.1", "10.10.10.10", "192.168.1.1", "8.8.8.8", "1.1.1.1"}
	for _, s := range cases {
		ip := mustParseIP(t, s)
		want := safehttp.IsBlocked(ip)
		if got := safehttp.IsBlockedForHost("whatever.example.com", ip); got != want {
			t.Errorf("IsBlockedForHost(host, %s) = %v, want %v (== IsBlocked)", s, got, want)
		}
	}
}

// TestIsBlockedForHostRequiresBothAllowlists is the core regression test
// for the vulnerability this function exists to close: an IP-only
// allowlist (SAFEHTTP_ALLOW_PRIVATE_IPS) lets ANY hostname that resolves
// to the allowed IP through, including hostnames the operator never
// intended to expose (e.g. an unrelated internal-only sibling service
// sharing the same gateway address). IsBlockedForHost must refuse any
// hostname not ALSO on SAFEHTTP_ALLOW_PRIVATE_HOSTS, even though the IP
// itself is allowlisted.
func TestIsBlockedForHostRequiresBothAllowlists(t *testing.T) {
	resetPrivateAllowlists(t)
	gatewayIP := mustParseIP(t, "10.10.10.10")
	safehttp.SetAllowedPrivateIPs([]net.IP{gatewayIP})
	safehttp.SetAllowedPrivateHosts([]string{"scrapetheworld.org", "okssh.com"})

	// Trusted hostname, allowed IP -> must be allowed.
	if safehttp.IsBlockedForHost("scrapetheworld.org", gatewayIP) {
		t.Error("trusted hostname resolving to the allowed IP must not be blocked")
	}
	if safehttp.IsBlockedForHost("www.okssh.com", gatewayIP) {
		t.Error("subdomain of a trusted hostname must not be blocked")
	}

	// Untrusted hostname that ALSO happens to resolve to the same
	// allowed gateway IP (e.g. an internal-only fleet vhost sharing the
	// gateway) -> must stay blocked. This is the exact pivot a plain
	// SAFEHTTP_ALLOW_PRIVATE_IPS=10.10.10.10 would have opened.
	if !safehttp.IsBlockedForHost("fleet-read.0exec.com", gatewayIP) {
		t.Error("SECURITY REGRESSION: untrusted hostname sharing the allowed gateway IP must remain blocked")
	}
	if !safehttp.IsBlockedForHost("fleet-secrets.0exec.com", gatewayIP) {
		t.Error("SECURITY REGRESSION: untrusted hostname sharing the allowed gateway IP must remain blocked")
	}

	// Suffix confusion: "notokssh.com" must NOT match the "okssh.com"
	// allowlist entry merely because it ends with the same characters.
	if !safehttp.IsBlockedForHost("notokssh.com", gatewayIP) {
		t.Error("suffix-similar hostname must not incorrectly match the allowlist")
	}
	if !safehttp.IsBlockedForHost("evilokssh.com", gatewayIP) {
		t.Error("suffix-similar hostname must not incorrectly match the allowlist")
	}
}

// TestIsBlockedForHostIPStillRequired proves the reverse pairing also
// matters: a trusted hostname resolving to a DIFFERENT private IP that is
// NOT on SAFEHTTP_ALLOW_PRIVATE_IPS must still be blocked. The hostname
// allowlist narrows the IP allowlist; it never substitutes for it.
func TestIsBlockedForHostIPStillRequired(t *testing.T) {
	resetPrivateAllowlists(t)
	safehttp.SetAllowedPrivateIPs([]net.IP{mustParseIP(t, "10.10.10.10")})
	safehttp.SetAllowedPrivateHosts([]string{"scrapetheworld.org"})

	otherPrivateIP := mustParseIP(t, "10.10.10.20")
	if !safehttp.IsBlockedForHost("scrapetheworld.org", otherPrivateIP) {
		t.Error("trusted hostname resolving to a NON-allowlisted private IP must still be blocked")
	}
}

// TestIsBlockedForHostNoHostAllowlistPreservesLegacyIPOnlyBehavior covers
// the back-compat path: a caller that only ever calls one fixed, known
// internal URL (e.g. go-fleet-secrets' documented usage) can set just
// SAFEHTTP_ALLOW_PRIVATE_IPS, with no host allowlist at all, and keep the
// original IP-only bypass semantics unchanged.
func TestIsBlockedForHostNoHostAllowlistPreservesLegacyIPOnlyBehavior(t *testing.T) {
	resetPrivateAllowlists(t)
	ip := mustParseIP(t, "10.10.10.10")
	safehttp.SetAllowedPrivateIPs([]net.IP{ip})
	// No SetAllowedPrivateHosts call: hasAllowedPrivateHosts() is false.

	if safehttp.IsBlockedForHost("literally-anything.example", ip) {
		t.Error("with no host allowlist configured, the legacy IP-only bypass must still apply")
	}
	if safehttp.IsBlocked(ip) {
		t.Error("sanity: IsBlocked itself must also allow the IP-allowlisted address")
	}
}

// Real hostname-resolution-path coverage for GuardHost (cache-sensitive;
// exercised together with defaultGuardCache resets) lives in
// allow_private_hosts_internal_test.go (package safehttp, white-box) so it
// can clear the package-global verdict cache between assertions the way
// dnsguardcache_rebind_test.go already does for the rebind tests.
