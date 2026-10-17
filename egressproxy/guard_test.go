package egressproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
)

func TestDefaultGuardVerdicts(t *testing.T) {
	denied := []string{
		"127.0.0.1", "127.8.9.10", "::1", "10.0.0.5", "172.16.3.4", "192.168.1.1",
		"169.254.169.254", "fe80::1", "100.64.0.1", "0.0.0.0", "fd00::1", "::ffff:127.0.0.1",
	}
	for _, h := range denied {
		if err := DefaultGuard(context.Background(), h, "443"); !errors.Is(err, ErrDenied) {
			t.Errorf("DefaultGuard(%q) = %v, want ErrDenied", h, err)
		}
	}
	for _, h := range []string{"8.8.8.8", "2001:4860:4860::8888", "93.184.216.34"} {
		if err := DefaultGuard(context.Background(), h, "443"); err != nil {
			t.Errorf("DefaultGuard(%q) = %v, want allowed", h, err)
		}
	}
	// A compliance-denylisted name is denied without any DNS lookup.
	if err := DefaultGuard(context.Background(), "rutracker.org", "443"); !errors.Is(err, ErrDenied) {
		t.Errorf("denylisted domain = %v, want ErrDenied", err)
	}
	if err := DefaultGuard(context.Background(), "", "443"); !errors.Is(err, ErrDenied) {
		t.Errorf("empty host = %v, want ErrDenied", err)
	}
}

func TestGuardDeniesInternalTargetsWith403(t *testing.T) {
	up := newFakeUpstream(t, fakeUser, fakePass)
	never := newEchoServer(t)
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.Guard = nil // the default guard
	})

	targets := []string{
		"127.0.0.1:" + portOf(never.addr()), "169.254.169.254:80", "10.1.2.3:443",
		"192.168.0.10:80", "[::1]:443", "100.64.0.1:80",
	}
	for _, target := range targets {
		_, resp, _ := connectVia(t, p.Addr(), target)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("CONNECT %s = %d, want 403", target, resp.StatusCode)
		}
	}
	client := proxyClient(t, p.Proxy)
	resp, err := client.Get("http://169.254.169.254/latest/meta-data/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("plain GET to the metadata address = %d, want 403", resp.StatusCode)
	}

	if up.requests() != 0 {
		t.Fatalf("a denied target reached the upstream proxy (%d requests)", up.requests())
	}
	if never.accepts.Load() != 0 {
		t.Fatal("a denied target was contacted")
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "guard_denied"), float64(len(targets)))
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("http", "guard_denied"), 1)
}

func TestGuardFailsClosedWhenNameDoesNotResolve(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.Guard = nil
	})
	_, resp, _ := connectVia(t, p.Addr(), "no-such-host.invalid:443")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (could not verify)", resp.StatusCode)
	}
	if up.requests() != 0 {
		t.Fatal("an unverified target reached the upstream")
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "guard_error"), 1)
}

// setAllowed installs safehttp allow-lists for one test. safehttp keeps them in
// package state, so these tests must not run in parallel.
func setAllowed(t *testing.T, ips []net.IP, hosts []string) {
	t.Helper()
	safehttp.SetAllowedPrivateIPs(ips)
	safehttp.SetAllowedPrivateHosts(hosts)
	t.Cleanup(func() {
		safehttp.SetAllowedPrivateIPs(nil)
		safehttp.SetAllowedPrivateHosts(nil)
	})
}

func TestGuardHonoursSafehttpAllowList(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	setAllowed(t, []net.IP{net.ParseIP("127.0.0.1")}, nil)
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.Guard = nil
	})

	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allow-listed 127.0.0.1 = %d, want 200", resp.StatusCode)
	}
	echoOnce(t, c, br, "allowed")

	// The IP allow-list alone does not open another private address.
	_, resp, _ = connectVia(t, p.Addr(), "10.9.9.9:80")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("10.9.9.9 = %d, want 403", resp.StatusCode)
	}
}

func TestGuardHostAllowListScopesTheIPAllowList(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	// With SAFEHTTP_ALLOW_PRIVATE_HOSTS set, an IP literal is not on it, so the
	// allow-listed IP is still refused for a bare-IP target.
	setAllowed(t, []net.IP{net.ParseIP("127.0.0.1")}, []string{"fleet.example"})
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.Guard = nil
	})
	_, resp, _ := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (IP not host-scoped)", resp.StatusCode)
	}
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func TestGuardVerdictsAreCachedPerHost(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var calls atomic.Int64
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.Now = clk.Now
		c.GuardCacheTTL = 30 * time.Second
		c.Guard = func(ctx context.Context, host, port string) error {
			calls.Add(1)
			return nil
		}
	})

	for i := 0; i < 5; i++ {
		c, resp, br := connectVia(t, p.Addr(), echo.addr())
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		echoOnce(t, c, br, "c")
		c.Close()
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("guard ran %d times for 5 requests to one host, want 1", n)
	}
	clk.Advance(31 * time.Second)
	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	echoOnce(t, c, br, "c")
	if n := calls.Load(); n != 2 {
		t.Fatalf("guard ran %d times after the TTL, want 2", n)
	}
}

func TestGuardCacheTTLsAndBound(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var calls int
	verdict := ErrDenied
	g := func(ctx context.Context, host, port string) error { calls++; return verdict }
	c := newGuardCache(30*time.Second, 3, clk.Now)
	ctx := context.Background()

	// Denials are cached as long as allows.
	for i := 0; i < 3; i++ {
		if err := c.check(ctx, g, "Blocked.Example.", "443"); !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (case and trailing dot share an entry)", calls)
	}

	// An "unavailable" verdict is only remembered briefly.
	calls = 0
	verdict = errors.Join(ErrGuardUnavailable, errors.New("dns"))
	c.check(ctx, g, "flaky.example", "443")
	c.check(ctx, g, "flaky.example", "443")
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 within the short TTL", calls)
	}
	clk.Advance(3 * time.Second)
	c.check(ctx, g, "flaky.example", "443")
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 after the short TTL", calls)
	}

	// The cache never grows past its bound.
	verdict = nil
	for i := 0; i < 50; i++ {
		c.check(ctx, g, "host"+string(rune('a'+i%26))+string(rune('a'+i/26))+".example", "80")
		if n := c.len(); n > 3 {
			t.Fatalf("cache holds %d entries, bound is 3", n)
		}
	}

	// A verdict reached after the request was abandoned is not remembered.
	calls = 0
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	c2 := newGuardCache(time.Minute, 10, clk.Now)
	c2.check(cctx, g, "gone.example", "80")
	if c2.len() != 0 {
		t.Fatal("verdict cached for a cancelled request")
	}
}
