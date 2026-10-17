package egressproxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCredentialsNeverAppearInLogsOrMetrics(t *testing.T) {
	const secret = "sup3r-s3cret-pw-9917"
	const user = "alice-the-account"
	good := newFakeUpstream(t, user, secret)
	wrongPass := newFakeUpstream(t, user, "another-password")
	bad5xx := newFakeUpstream(t, user, secret)
	bad5xx.setConnectStatus(http.StatusBadGateway)
	echo := newEchoServer(t)
	deadAddr := closedAddr(t)
	hang := hangListener(t)

	urls := []string{
		good.url(),
		fmt.Sprintf("http://%s:%s@%s", user, secret, wrongPass.addr()),
		bad5xx.url(),
		fmt.Sprintf("http://%s:%s@%s", user, secret, deadAddr),
		fmt.Sprintf("http://%s:%s@%s", user, secret, hang),
		fmt.Sprintf("http://%s:%s@bad host:80", user, secret), // url.Parse quotes the whole URL in its error
		fmt.Sprintf("socks5://%s:%s@127.0.0.1:1080", user, secret),
		fmt.Sprintf("http://%s:%s@", user, secret),
	}
	for _, raw := range urls {
		p := startProxy(t, func(c *Config) {
			c.Supplier = newSupplier(raw, raw)
			c.DialTimeout = 400 * time.Millisecond // keeps the hanging cases short
			c.IdleTimeout = 500 * time.Millisecond
		})
		_, resp, _ := connectVia(t, p.Addr(), echo.addr())
		resp.Body.Close()
		client := proxyClient(t, p.Proxy)
		if r, err := client.Get("http://origin.invalid/"); err == nil {
			r.Body.Close()
		}

		out := p.logs.String()
		assertNoSecret(t, "logs", out, user, secret)
		families, _ := p.reg.Gather()
		var sb strings.Builder
		for _, mf := range families {
			sb.WriteString(mf.String())
		}
		assertNoSecret(t, "metrics", sb.String(), user, secret)
	}

	// The log is not empty by accident: a failing upstream is reported by
	// host:port and result.
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(fmt.Sprintf("http://%s:%s@%s", user, secret, deadAddr)) })
	_, resp, _ := connectVia(t, p.Addr(), echo.addr())
	resp.Body.Close()
	out := p.logs.String()
	if !strings.Contains(out, deadAddr) || !strings.Contains(out, "dial_error") {
		t.Fatalf("log does not name the failing upstream and result:\n%s", out)
	}
	assertNoSecret(t, "logs", out, user, secret)
}

func assertNoSecret(t *testing.T, where, text, user, secret string) {
	t.Helper()
	for _, needle := range []string{secret, user, basicHeader(user, secret), strings.TrimPrefix(basicHeader(user, secret), "Basic ")} {
		if strings.Contains(text, needle) {
			t.Fatalf("%s contain %q:\n%s", where, needle, text)
		}
	}
}

func TestConcurrentTunnelsAndRequests(t *testing.T) {
	up := newFakeUpstream(t, fakeUser, fakePass)
	echo := newEchoServer(t)
	sup := newSupplier(up.url())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })
	client := proxyClient(t, p.Proxy)

	const workers = 40
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c, resp, br := connectViaE(p.Addr(), echo.addr())
			if resp == nil || resp.StatusCode != 200 {
				errs <- fmt.Errorf("worker %d: connect failed (%v)", i, resp)
				return
			}
			defer c.Close()
			for j := 0; j < 5; j++ {
				msg := fmt.Sprintf("worker-%d-msg-%d", i, j)
				io.WriteString(c, msg)
				buf := make([]byte, len(msg))
				if _, err := io.ReadFull(br, buf); err != nil || string(buf) != msg {
					errs <- fmt.Errorf("worker %d: echo %q err=%v", i, buf, err)
					return
				}
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				resp, err := client.Get(fmt.Sprintf("http://origin.invalid/%d/%d", i, j))
				if err != nil {
					errs <- err
					return
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if want := fmt.Sprintf("saw GET http://origin.invalid/%d/%d len=0", i, j); string(b) != want {
					errs <- fmt.Errorf("plain %d/%d = %q", i, j, b)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "ok"), workers)
	waitMetric(t, p.reg, "egressproxy_active_tunnels", nil, 0)
	if got := len(up.targets()); got != workers {
		t.Fatalf("upstream saw %d tunnels, want %d", got, workers)
	}
}

func TestSecondProxyReusesRegisteredMetrics(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	p1 := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })
	// Same registry: the collectors already exist and must be reused.
	p2 := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.Registerer = p1.reg
	})
	for _, p := range []*testProxy{p1, p2} {
		c, resp, br := connectVia(t, p.Addr(), echo.addr())
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		echoOnce(t, c, br, "m")
		c.Close()
	}
	waitMetric(t, p1.reg, "egressproxy_requests_total", reqLabels("connect", "ok"), 2)
}

func TestClientBodyFailureDoesNotBlameTheUpstream(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	sup := newSupplier(up.url())
	p := startProxy(t, func(c *Config) {
		c.Supplier = sup
		c.MaxTunnelBytes = 100
	})
	client := proxyClient(t, p.Proxy)

	// A 5000 byte upload against a 100 byte cap.
	resp, err := client.Post("http://origin.invalid/up", "text/plain", strings.NewReader(strings.Repeat("u", 5000)))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("http", "byte_cap"), 1)
	for _, m := range sup.Marks() {
		if !m.ok {
			t.Fatalf("an over-cap upload was held against the exit IP: %v", sup.Marks())
		}
	}
}
