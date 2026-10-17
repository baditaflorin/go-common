package egressproxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMaxConnsOverloadAnswers503Fast(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.MaxConns = 2
	})

	var held []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", p.Addr())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	eventually(t, 2*time.Second, "two sessions", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.sessions) == 2
	})

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "too many connections") {
		t.Fatalf("overload answer = %d %q", resp.StatusCode, body)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("overload answer took %v, want immediate", d)
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("unknown", "overload"), 1)

	// Capacity comes back when a connection goes away.
	held[0].Close()
	eventually(t, 3*time.Second, "a freed slot to serve a tunnel", func() bool {
		cc, err := net.Dial("tcp", p.Addr())
		if err != nil {
			return false
		}
		defer cc.Close()
		_ = cc.SetDeadline(time.Now().Add(2 * time.Second))
		io.WriteString(cc, "CONNECT "+echo.addr()+" HTTP/1.1\r\nHost: x\r\n\r\n")
		r, err := http.ReadResponse(bufio.NewReader(cc), &http.Request{Method: "CONNECT"})
		return err == nil && r.StatusCode == 200
	})
	held[1].Close()
}

func TestMaxTunnelBytesClosesTheTunnel(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.MaxTunnelBytes = 1000
	})
	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	go func() { // 4000 bytes up, 1000 allowed
		io.WriteString(c, strings.Repeat("a", 4000))
	}()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _ := io.Copy(io.Discard, br)
	if n > 1000 {
		t.Fatalf("received %d bytes back through a 1000 byte cap", n)
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "byte_cap"), 1)
	waitMetric(t, p.reg, "egressproxy_active_tunnels", nil, 0)
	if up := metricValue(p.reg, "egressproxy_bytes_total", map[string]string{"direction": "up"}); up > 1000 {
		t.Fatalf("bytes up = %v, want at most the cap", up)
	}
}

func TestMaxTunnelBytesZeroMeansUnlimited(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.MaxTunnelBytes = 0
	})
	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	echoOnce(t, c, br, strings.Repeat("z", 200_000))
}

func TestMaxBytesAppliesToPlainResponses(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.MaxTunnelBytes = 1000
	})
	client := proxyClient(t, p.Proxy)
	resp, err := client.Get("http://origin.invalid/big") // 5000 byte body
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("a 5000 byte response passed a 1000 byte cap intact")
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("http", "byte_cap"), 1)
}

func TestIdleTimeoutClosesQuietTunnels(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.IdleTimeout = 300 * time.Millisecond
	})
	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Traffic in either direction keeps the tunnel alive past the idle time.
	for i := 0; i < 6; i++ {
		echoOnce(t, c, br, "tick")
		time.Sleep(120 * time.Millisecond)
	}
	// Then it goes quiet and is closed.
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("read data from a quiet tunnel")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the idle tunnel was not closed")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("idle close took %v for a 300ms timeout", d)
	}
	waitMetric(t, p.reg, "egressproxy_active_tunnels", nil, 0)
}

// goroutinesInProxy lists goroutines that are inside this package's proxy
// code (not its tests).
func goroutinesInProxy() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, marker := range []string{"egressproxy.(*session)", "egressproxy.(*Proxy)", "egressproxy.copyCapped"} {
			if strings.Contains(g, marker) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

func TestCloseWithOpenTunnelsLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	var closedAddr string

	// Everything the scenario creates (fakes, listeners, clients) is released
	// when the subtest ends, so only the proxy can account for a leftover.
	t.Run("scenario", func(t *testing.T) {
		up := newFakeUpstream(t, fakeUser, fakePass)
		echo := newEchoServer(t)
		hang := hangListener(t)
		p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })
		closedAddr = p.Addr()

		var clients []net.Conn
		for i := 0; i < 5; i++ {
			c, resp, br := connectVia(t, p.Addr(), echo.addr())
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			echoOnce(t, c, br, "open")
			clients = append(clients, c)
		}
		// A client that connected but has not sent its request, and a
		// handshake stuck on a hanging upstream.
		idle, err := net.Dial("tcp", p.Addr())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, idle)
		stuck := startProxy(t, func(c *Config) {
			c.Supplier = newSupplier("http://" + hang)
			c.DialTimeout = 30 * time.Second
		})
		stuckConn, err := net.Dial("tcp", stuck.Addr())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, stuckConn)
		io.WriteString(stuckConn, "CONNECT "+echo.addr()+" HTTP/1.1\r\nHost: x\r\n\r\n")
		time.Sleep(100 * time.Millisecond)

		for name, px := range map[string]*Proxy{"main": p.Proxy, "stuck": stuck.Proxy} {
			done := make(chan struct{})
			go func() { px.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("Close of the %s proxy blocked with open tunnels", name)
			}
		}
		// The clients see their connections end (a refusal written on the way
		// out is fine; the stream must then reach EOF).
		for _, c := range clients {
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.Copy(io.Discard, c); err != nil {
				t.Errorf("a connection survived Close: %v", err)
			}
			c.Close()
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		gs, n := goroutinesInProxy(), runtime.NumGoroutine()
		if len(gs) == 0 && n <= before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines (was %d); in proxy code:\n%s", n, before, strings.Join(gs, "\n\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := net.DialTimeout("tcp", closedAddr, 200*time.Millisecond); err == nil {
		t.Fatal("proxy still accepts after Close")
	}
}
