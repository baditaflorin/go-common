package egressproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestStartListensOnLoopbackOnly(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })

	host, port, err := net.SplitHostPort(p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" || port == "" || port == "0" {
		t.Fatalf("Addr = %q, want 127.0.0.1:<port>", p.Addr())
	}
	if want := "http://" + p.Addr(); p.URL() != want {
		t.Fatalf("URL = %q, want %q", p.URL(), want)
	}
	if ip := p.ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() || ip.To4() == nil {
		t.Fatalf("listener bound to %v, want IPv4 loopback", ip)
	}
}

func TestStartRequireUpstreamWithoutSupplier(t *testing.T) {
	cfg := DefaultConfig()
	_, err := Start(context.Background(), cfg)
	if !errors.Is(err, ErrNoSupplier) {
		t.Fatalf("Start without Supplier = %v, want ErrNoSupplier", err)
	}
}

func TestStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := DefaultConfig()
	cfg.RequireUpstream = false
	cfg.Registerer = prometheus.NewRegistry()
	p, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	addr := p.Addr()
	cancel()
	eventually(t, 3*time.Second, "listener to close after ctx cancel", func() bool {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		return false
	})
	_ = p.Close() // idempotent
}

func TestConnectInjectsCredentials(t *testing.T) {
	up := newFakeUpstream(t, fakeUser, fakePass)
	echo := newEchoServer(t)
	sup := newSupplier(up.url())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })

	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	echoOnce(t, c, br, "hello through the tunnel")
	echoOnce(t, c, br, "and again")

	if got := up.targets(); len(got) != 1 || got[0] != echo.addr() {
		t.Fatalf("upstream saw CONNECT targets %v, want [%s]", got, echo.addr())
	}
	if got := up.auths(); len(got) != 1 || got[0] != basicHeader(fakeUser, fakePass) {
		t.Fatalf("upstream saw Proxy-Authorization %v, want the injected Basic header", got)
	}
	if m := sup.Marks(); len(m) != 1 || m[0] != (mark{up.addr(), true}) {
		t.Fatalf("MarkResult calls = %v, want one success for %s", m, up.addr())
	}
	waitMetric(t, p.reg, "egressproxy_active_tunnels", nil, 1)

	c.Close()
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "ok"), 1)
	waitMetric(t, p.reg, "egressproxy_active_tunnels", nil, 0)
	waitMetric(t, p.reg, "egressproxy_upstream_attempts_total", map[string]string{"result": "ok"}, 1)
	if up := metricValue(p.reg, "egressproxy_bytes_total", map[string]string{"direction": "up"}); up < float64(len("hello through the tunnel")) {
		t.Fatalf("bytes up = %v, want at least the payload", up)
	}
	if down := metricValue(p.reg, "egressproxy_bytes_total", map[string]string{"direction": "down"}); down < float64(len("hello through the tunnel")) {
		t.Fatalf("bytes down = %v, want at least the payload", down)
	}
	if n := metricValue(p.reg, "egressproxy_request_duration_seconds", map[string]string{"kind": "connect"}); n != 1 {
		t.Fatalf("duration histogram count = %v, want 1", n)
	}
}

func TestConnectWithoutUpstreamCredentials(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })

	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	echoOnce(t, c, br, "x")
	if got := up.auths(); len(got) != 1 || got[0] != "" {
		t.Fatalf("Proxy-Authorization sent without credentials: %v", got)
	}
}

func TestConnectWrongCredentialsIs502AndMarksFailure(t *testing.T) {
	up := newFakeUpstream(t, fakeUser, fakePass)
	echo := newEchoServer(t)
	bad := fmt.Sprintf("http://%s:wrong-password@%s", fakeUser, up.addr())
	sup := newSupplier(bad)
	p := startProxy(t, func(c *Config) { c.Supplier = sup })

	_, resp, _ := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (a 407 must not reach the browser)", resp.StatusCode)
	}
	if resp.Header.Get("Proxy-Authenticate") != "" {
		t.Fatal("Proxy-Authenticate leaked to the browser")
	}
	marks := sup.Marks()
	if len(marks) == 0 {
		t.Fatal("MarkResult was not called")
	}
	for _, m := range marks {
		if m.ok || m.addr != up.addr() {
			t.Fatalf("MarkResult = %+v, want (%s, false)", m, up.addr())
		}
	}
	if echo.accepts.Load() != 0 {
		t.Fatal("target was contacted although the upstream refused")
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "upstream_error"), 1)
	waitMetric(t, p.reg, "egressproxy_upstream_attempts_total", map[string]string{"result": "auth_failed"}, 2)
}

func TestPlainHTTPForwarding(t *testing.T) {
	up := newFakeUpstream(t, fakeUser, fakePass)
	sup := newSupplier(up.url())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })
	client := proxyClient(t, p.Proxy)

	req, _ := http.NewRequest("GET", "http://origin.invalid/a/b?x=1", nil)
	req.Header.Set("X-Test", "kept")
	req.Header.Set("Proxy-Authorization", "Basic ZXZpbDpldmls") // browser-side, must not reach the upstream
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "saw GET http://origin.invalid/a/b?x=1 len=0" {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	for _, h := range []string{"Proxy-Authenticate", "Keep-Alive"} {
		if resp.Header.Get(h) != "" {
			t.Errorf("hop-by-hop response header %s leaked to the browser", h)
		}
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Error("end-to-end response header was dropped")
	}

	seen := up.plainSeen()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d plain requests", len(seen))
	}
	h := seen[0].Header
	if got := h.Get("Proxy-Authorization"); got != basicHeader(fakeUser, fakePass) {
		t.Errorf("upstream Proxy-Authorization = %q, want the injected one (not the browser's)", got)
	}
	if h.Get("Proxy-Connection") != "" || h.Get("X-Hop") != "" {
		t.Errorf("hop-by-hop request headers reached the upstream: %v", h)
	}
	if h.Get("X-Test") != "kept" {
		t.Errorf("end-to-end request header dropped: %v", h)
	}
	if seen[0].Host != "origin.invalid" {
		t.Errorf("Host = %q", seen[0].Host)
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("http", "ok"), 1)
	if m := sup.Marks(); len(m) != 1 || !m[0].ok {
		t.Fatalf("MarkResult calls = %v", m)
	}
}

func TestPlainHTTPKeepAliveToBrowser(t *testing.T) {
	up := newFakeUpstream(t, fakeUser, fakePass)
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })
	client := proxyClient(t, p.Proxy)

	var reused []bool
	for i := 0; i < 3; i++ {
		var wasReused bool
		trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { wasReused = i.Reused }}
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://origin.invalid/%d", i), nil)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		reused = append(reused, wasReused)
	}
	if reused[0] || !reused[1] || !reused[2] {
		t.Fatalf("browser connection reuse = %v, want [false true true]", reused)
	}
}

func TestPlainHTTPRequestBodies(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })
	client := proxyClient(t, p.Proxy)

	// Content-Length body.
	resp, err := client.Post("http://origin.invalid/form", "text/plain", strings.NewReader("hello body"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "saw POST http://origin.invalid/form len=10" {
		t.Fatalf("post response = %q", got)
	}
	// Chunked body (unknown length).
	resp, err = client.Post("http://origin.invalid/streamed", "text/plain", io.NopCloser(strings.NewReader("streamed body")))
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "saw POST http://origin.invalid/streamed len=13" {
		t.Fatalf("chunked response = %q", got)
	}
	seen := up.plainSeen()
	if len(seen) != 2 || seen[0].Body != "hello body" || seen[1].Body != "streamed body" {
		t.Fatalf("upstream bodies = %+v", seen)
	}
}

func TestPlainHTTPExpectContinue(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "POST http://origin.invalid/up HTTP/1.1\r\nHost: origin.invalid\r\nExpect: 100-continue\r\nContent-Length: 5\r\n\r\n")
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 100") {
		t.Fatalf("interim answer = %q, %v; want 100 Continue before the body", line, err)
	}
	br.ReadString('\n') // blank line
	fmt.Fprint(c, "12345")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "saw POST http://origin.invalid/up len=5" {
		t.Fatalf("response = %q", body)
	}
	if up.plainSeen()[0].Header.Get("Expect") != "" {
		t.Fatal("Expect was forwarded to the upstream")
	}
}

func TestWebSocketUpgradeOverPlainHTTP(t *testing.T) {
	up := newFakeUpstream(t, fakeUser, fakePass)
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "GET http://ws.invalid/chat HTTP/1.1\r\nHost: ws.invalid\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!strings.EqualFold(resp.Header.Get("Connection"), "Upgrade") {
		t.Fatalf("handshake = %d %v", resp.StatusCode, resp.Header)
	}
	echoOnce(t, c, br, "frame-1")
	echoOnce(t, c, br, "frame-2")

	up.mu.Lock()
	hdr := up.upgrades[0]
	up.mu.Unlock()
	if hdr.Get("Proxy-Authorization") != basicHeader(fakeUser, fakePass) || !strings.EqualFold(hdr.Get("Connection"), "Upgrade") ||
		hdr.Get("Sec-WebSocket-Key") == "" {
		t.Fatalf("upstream saw %v", hdr)
	}
	c.Close()
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("http", "ok"), 1)
}

func TestBadRequests(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })

	cases := map[string]string{
		"origin-form":      "GET /metrics HTTP/1.1\r\nHost: x\r\n\r\n",
		"connect no port":  "CONNECT example.com HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"connect bad port": "CONNECT example.com:99999 HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"https absolute":   "GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"garbage":          "this is not http\r\n\r\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := net.Dial("tcp", p.Addr())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			io.WriteString(c, raw)
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
	if up.requests() != 0 {
		t.Fatal("a bad request reached the upstream")
	}
}

func TestOversizeHeaderRefused(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	go func() {
		io.WriteString(c, "GET http://x.invalid/ HTTP/1.1\r\nHost: x.invalid\r\nX-Big: ")
		chunk := strings.Repeat("a", 64<<10)
		for i := 0; i < 40; i++ { // 2.5 MiB, over the 1 MiB limit
			if _, err := io.WriteString(c, chunk); err != nil {
				return
			}
		}
	}()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err == nil && resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want 431", resp.StatusCode)
	}
	if up.requests() != 0 {
		t.Fatal("an oversize request reached the upstream")
	}
}

func TestReadHeaderTimeoutClosesSlowClient(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier(up.url())
		c.ReadHeaderTimeout = 200 * time.Millisecond
	})
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT example.com:44") // never finished
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("read succeeded, want the connection closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("proxy did not close a client stuck in the request line")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("closed after %v", d)
	}
}

func TestKeyFuncUsesKeyedSupplier(t *testing.T) {
	keyedUp := newFakeUpstream(t, "", "")
	plainUp := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	ks := &keyedTestSupplier{testSupplier: newSupplier(plainUp.url()), keyed: keyedUp.url()}
	p := startProxy(t, func(c *Config) {
		c.Supplier = ks
		c.KeyFunc = func(tg Target) string {
			if tg.Kind != "connect" || tg.Client == nil {
				t.Errorf("Target = %+v", tg)
			}
			return tg.Host
		}
	})

	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	echoOnce(t, c, br, "k")
	if len(keyedUp.targets()) != 1 || len(plainUp.targets()) != 0 {
		t.Fatalf("keyed upstream saw %v, plain upstream saw %v", keyedUp.targets(), plainUp.targets())
	}
	ks.mu.Lock()
	keys := append([]string(nil), ks.keys...)
	ks.mu.Unlock()
	if len(keys) != 1 || keys[0] != "127.0.0.1" {
		t.Fatalf("ProxyURLForKey keys = %v", keys)
	}
}

func TestKeyedFailureRetriesOnUnkeyedUpstream(t *testing.T) {
	keyedUp := newFakeUpstream(t, "", "")
	keyedUp.setConnectStatus(http.StatusServiceUnavailable)
	plainUp := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	ks := &keyedTestSupplier{testSupplier: newSupplier(plainUp.url()), keyed: keyedUp.url()}
	p := startProxy(t, func(c *Config) {
		c.Supplier = ks
		c.KeyFunc = func(tg Target) string { return tg.Host }
	})

	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the retry to succeed", resp.StatusCode)
	}
	echoOnce(t, c, br, "r")
	marks := ks.Marks()
	if len(marks) != 2 || marks[0] != (mark{keyedUp.addr(), false}) || marks[1] != (mark{plainUp.addr(), true}) {
		t.Fatalf("marks = %v", marks)
	}
}

func TestConfigFromEnv(t *testing.T) {
	d := ConfigFromEnv()
	if d.MaxConns != 256 || d.DialTimeout != 10*time.Second || d.IdleTimeout != 60*time.Second ||
		d.ReadHeaderTimeout != 15*time.Second || d.MaxTunnelBytes != 64<<20 || !d.RequireUpstream || d.IgnoreUpstream5xx {
		t.Fatalf("defaults = %+v", d)
	}

	t.Setenv(EnvMaxConns, "32")
	t.Setenv(EnvDialTimeout, "3s")
	t.Setenv(EnvIdleTimeout, "2m")
	t.Setenv(EnvReadHeaderTimeout, "5s")
	t.Setenv(EnvMaxTunnelBytes, "0")
	t.Setenv(EnvRequireUpstream, "false")
	t.Setenv(EnvIgnoreUpstream5xx, "true")
	t.Setenv(EnvGuardCacheTTL, "1s")
	c := ConfigFromEnv()
	if c.MaxConns != 32 || c.DialTimeout != 3*time.Second || c.IdleTimeout != 2*time.Minute ||
		c.ReadHeaderTimeout != 5*time.Second || c.MaxTunnelBytes != 0 || c.RequireUpstream || !c.IgnoreUpstream5xx ||
		c.GuardCacheTTL != time.Second {
		t.Fatalf("overrides = %+v", c)
	}

	// Garbage and out-of-range values keep the defaults.
	t.Setenv(EnvMaxConns, "-4")
	t.Setenv(EnvDialTimeout, "soon")
	t.Setenv(EnvIdleTimeout, "-1s")
	t.Setenv(EnvMaxTunnelBytes, "-1")
	t.Setenv(EnvRequireUpstream, "maybe")
	c = ConfigFromEnv()
	if c.MaxConns != 256 || c.DialTimeout != 10*time.Second || c.IdleTimeout != 60*time.Second ||
		c.MaxTunnelBytes != 64<<20 || !c.RequireUpstream {
		t.Fatalf("invalid values were not ignored: %+v", c)
	}
}

func TestPlainHTTPResponseFraming(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })
	client := proxyClient(t, p.Proxy)

	// Chunked, with the connection reused afterwards.
	resp, err := client.Get("http://origin.invalid/chunked")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "alpha-beta-gamma" || len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("chunked response = %q, TE %v", b, resp.TransferEncoding)
	}
	// HEAD carries headers only.
	resp, err = client.Head("http://origin.invalid/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(b) != 0 || resp.ContentLength <= 0 {
		t.Fatalf("HEAD = %d, %d body bytes, Content-Length %d", resp.StatusCode, len(b), resp.ContentLength)
	}
	// 204 has no body and no framing headers.
	resp, err = client.Get("http://origin.invalid/nocontent")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 204 || len(b) != 0 {
		t.Fatalf("204 = %d %q", resp.StatusCode, b)
	}
	// And after all of that the same browser connection still works.
	var reused bool
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}
	req, _ := http.NewRequest("GET", "http://origin.invalid/again", nil)
	resp, err = client.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !reused {
		t.Fatal("the browser connection was not kept alive across framed responses")
	}
}

func TestPlainHTTPCloseDelimitedResponse(t *testing.T) {
	// An upstream that answers without any length and closes: the body can only
	// end with the connection, so the browser connection must close too.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\nread until close")
			}()
		}
	}()
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier("http://" + ln.Addr().String()) })
	client := proxyClient(t, p.Proxy)
	resp, err := client.Get("http://origin.invalid/stream")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(b) != "read until close" {
		t.Fatalf("body = %q, err %v", b, err)
	}
	if !resp.Close {
		t.Fatal("close-delimited response was not marked Connection: close for the browser")
	}
}

func TestPlainHTTPClientConnectionClose(t *testing.T) {
	up := newFakeUpstream(t, "", "")
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "GET http://origin.invalid/a HTTP/1.1\r\nHost: origin.invalid\r\nConnection: close\r\n\r\n")
	b, err := io.ReadAll(c) // returns only when the proxy closes the connection
	if err != nil || !strings.Contains(string(b), "saw GET http://origin.invalid/a len=0") {
		t.Fatalf("read %q, %v", b, err)
	}
}

func TestHTTPSThroughTheTunnel(t *testing.T) {
	// A real TLS handshake (HTTP/2 negotiated) travels through the CONNECT tunnel
	// untouched: the proxy only relays bytes.
	up := newFakeUpstream(t, fakeUser, fakePass)
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello over %s %s", r.Proto, r.TLS.ServerName)
	}))
	target.EnableHTTP2 = true
	target.StartTLS()
	t.Cleanup(target.Close)
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })

	pu, _ := url.Parse(p.URL())
	pool := x509.NewCertPool()
	pool.AddCert(target.Certificate())
	tr := &http.Transport{
		Proxy:             http.ProxyURL(pu),
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: true,
	}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := client.Get(target.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(string(b), "hello over HTTP/2.0") {
		t.Fatalf("response = %d %q", resp.StatusCode, b)
	}
	if got := up.targets(); len(got) != 1 || got[0] != strings.TrimPrefix(target.URL, "https://") {
		t.Fatalf("upstream CONNECT targets = %v", got)
	}
}

func TestStartWithCancelledContextFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := DefaultConfig()
	cfg.RequireUpstream = false
	cfg.Registerer = prometheus.NewRegistry()
	if p, err := Start(ctx, cfg); err == nil {
		p.Close()
		t.Fatal("Start with a cancelled context succeeded")
	}
}
