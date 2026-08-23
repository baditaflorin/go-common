package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetPrivateAllowlistsInternal mirrors resetPrivateAllowlists in the
// black-box test package (allow_private_hosts_test.go), for the white-box
// tests in this file that also need to touch the unexported guard cache.
func resetPrivateAllowlistsInternal(t *testing.T) {
	t.Helper()
	SetAllowedPrivateIPs(nil)
	SetAllowedPrivateHosts(nil)
	clearGuardCache(t)
	t.Cleanup(func() {
		SetAllowedPrivateIPs(nil)
		SetAllowedPrivateHosts(nil)
		clearGuardCache(t)
	})
}

// TestGuardHostHonorsPrivateHostAllowlist exercises the real
// hostname-resolution path (not just the unit-level IsBlockedForHost)
// using "localhost", which reliably resolves to 127.0.0.1/::1 without a
// network dependency. The guard cache is cleared between assertions so a
// verdict from an earlier allowlist state can't leak into the next check.
func TestGuardHostHonorsPrivateHostAllowlist(t *testing.T) {
	resetPrivateAllowlistsInternal(t)

	// Baseline: localhost is blocked with nothing configured.
	if err := GuardHost(context.Background(), "localhost"); err == nil {
		t.Fatal("GuardHost(localhost): expected blocked error with no allowlist configured")
	}
	clearGuardCache(t)

	SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")})
	SetAllowedPrivateHosts([]string{"localhost"})

	if err := GuardHost(context.Background(), "localhost"); err != nil {
		t.Errorf("GuardHost(localhost): expected allowed once both allowlists match, got %v", err)
	}
	clearGuardCache(t)

	// A different hostname must not ride along just because an operator
	// allowlisted 127.0.0.1/::1 for "localhost".
	SetAllowedPrivateHosts([]string{"only-this-host.invalid"})
	if err := GuardHost(context.Background(), "localhost"); err == nil {
		t.Error("GuardHost(localhost): must be blocked again once localhost is no longer on the host allowlist, even though its IP still is")
	}
}

// TestRebindHostAllowlistedButIPRebinds is the host-allowlist analogue of
// TestRebindCachedAllowStillBlockedByControl: even a hostname that IS on
// SAFEHTTP_ALLOW_PRIVATE_HOSTS must not get a free pass at dial time if it
// (or a cached verdict for it) resolves somewhere other than the
// specifically allowlisted IP. The Dialer.Control re-check calls
// IsBlockedForHost with the ACTUAL connected address, so a rebind to any
// IP outside SAFEHTTP_ALLOW_PRIVATE_IPS is still blocked even though the
// hostname itself is trusted.
func TestRebindHostAllowlistedButIPRebinds(t *testing.T) {
	resetPrivateAllowlistsInternal(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("parse test server addr: %v", err)
	}

	const host = "localhost" // dial-time address is 127.0.0.1

	// Trust the hostname, but only pair it with a DIFFERENT IP
	// (10.10.10.10) than the one it will actually dial (127.0.0.1) — the
	// gateway IP an operator would legitimately allowlist, standing in for
	// "not 127.0.0.1". Also poison the GuardHost cache with a stale allow,
	// mirroring the existing rebind test's methodology.
	SetAllowedPrivateHosts([]string{host})
	SetAllowedPrivateIPs([]net.IP{net.ParseIP("10.10.10.10")})
	defaultGuardCache.put(host, nil)

	dial := makeDialer(false)
	conn, derr := dial(context.Background(), "tcp", host+":"+port)
	if conn != nil {
		conn.Close()
	}
	if derr == nil {
		t.Fatal("REBIND HOLE: trusted hostname reached a private IP outside SAFEHTTP_ALLOW_PRIVATE_IPS")
	}
	if !errors.Is(derr, ErrBlocked) {
		t.Fatalf("expected ErrBlocked from Control re-check, got: %v", derr)
	}
}

// TestDialerAllowsTrustedHostAndIPPair is the positive-path companion:
// once both allowlists agree, the dialer must actually be able to connect
// (proving the feature works end-to-end through makeDialer, not just at
// the GuardHost pre-check).
func TestDialerAllowsTrustedHostAndIPPair(t *testing.T) {
	resetPrivateAllowlistsInternal(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("parse test server addr: %v", err)
	}

	const host = "localhost"
	SetAllowedPrivateHosts([]string{host})
	SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")})

	dial := makeDialer(false)
	conn, derr := dial(context.Background(), "tcp", host+":"+port)
	if derr != nil {
		t.Fatalf("expected the trusted (host, ip) pair to connect, got error: %v", derr)
	}
	if conn != nil {
		conn.Close()
	}
}
