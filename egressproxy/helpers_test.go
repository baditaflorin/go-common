package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Fake credentials: tests never see a real proxy account.
const (
	fakeUser = "tester"
	fakePass = "not-a-real-pass-123"
)

// allowAll is the guard for tests that point the proxy at local fakes; the
// guard tests use DefaultGuard or their own.
func allowAll(context.Context, string, string) error { return nil }

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testSupplier hands out its URLs in order (wrapping) and records MarkResult.
type testSupplier struct {
	mu    sync.Mutex
	urls  []string
	i     int
	marks []mark
}

type mark struct {
	addr string
	ok   bool
}

func newSupplier(urls ...string) *testSupplier { return &testSupplier{urls: urls} }

func (s *testSupplier) Name() string { return "test" }

func (s *testSupplier) ProxyURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.urls) == 0 {
		return ""
	}
	u := s.urls[s.i%len(s.urls)]
	s.i++
	return u
}

func (s *testSupplier) MarkResult(addr string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks = append(s.marks, mark{addr, ok})
}

func (s *testSupplier) Marks() []mark {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mark(nil), s.marks...)
}

// keyedTestSupplier also implements proxysupplier.KeyedSupplier.
type keyedTestSupplier struct {
	*testSupplier
	keyed string
	mu    sync.Mutex
	keys  []string
}

func (k *keyedTestSupplier) ProxyURLForKey(key string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, key)
	return k.keyed
}

// funcSupplier answers from a function and has no ResultReporter.
type funcSupplier func() string

func (f funcSupplier) Name() string     { return "func" }
func (f funcSupplier) ProxyURL() string { return f() }

// seenRequest is what a fake upstream saw of a plain HTTP request.
type seenRequest struct {
	Method string
	URL    string
	Host   string
	Header http.Header
	Body   string
	TE     []string
}

// fakeUpstream is an upstream proxy built on net/http with hijacking. It
// requires Basic auth when user is set, tunnels CONNECT to the requested
// target, answers plain requests itself and echoes WebSocket upgrades.
type fakeUpstream struct {
	srv  *httptest.Server
	user string
	pass string

	mu             sync.Mutex
	connectTargets []string
	authHeaders    []string
	plain          []seenRequest
	upgrades       []http.Header
	connectStatus  int // answer CONNECT with this status instead of tunnelling
}

func newFakeUpstream(t *testing.T, user, pass string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{user: user, pass: pass}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) addr() string { return f.srv.Listener.Addr().String() }

// url is the credentialed upstream URL a supplier would return.
func (f *fakeUpstream) url() string {
	if f.user == "" {
		return "http://" + f.addr()
	}
	return fmt.Sprintf("http://%s:%s@%s", f.user, f.pass, f.addr())
}

func (f *fakeUpstream) setConnectStatus(code int) {
	f.mu.Lock()
	f.connectStatus = code
	f.mu.Unlock()
}

func (f *fakeUpstream) targets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.connectTargets...)
}

func (f *fakeUpstream) auths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authHeaders...)
}

func (f *fakeUpstream) plainSeen() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.plain...)
}

func (f *fakeUpstream) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.authHeaders)
}

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func (f *fakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.authHeaders = append(f.authHeaders, r.Header.Get("Proxy-Authorization"))
	f.mu.Unlock()
	if f.user != "" && r.Header.Get("Proxy-Authorization") != basicHeader(f.user, f.pass) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="fake"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	switch {
	case r.Method == http.MethodConnect:
		f.connect(w, r)
	case strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
		f.upgrade(w, r)
	default:
		f.servePlain(w, r)
	}
}

func (f *fakeUpstream) connect(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.connectTargets = append(f.connectTargets, r.Host)
	status := f.connectStatus
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	target, err := net.DialTimeout("tcp", r.Host, 2*time.Second)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		target.Close()
		return
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	splice(conn, rw.Reader, target)
}

// splice copies both ways with half-close and returns when both ended.
func splice(a net.Conn, aSrc io.Reader, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, aSrc)
		if tc, ok := b.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		if tc, ok := a.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	wg.Wait()
	a.Close()
	b.Close()
}

func (f *fakeUpstream) servePlain(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.plain = append(f.plain, seenRequest{
		Method: r.Method, URL: r.URL.String(), Host: r.Host,
		Header: r.Header.Clone(), Body: string(body), TE: r.TransferEncoding,
	})
	f.mu.Unlock()
	// Headers the proxy must not pass on to the browser.
	w.Header().Set("Proxy-Authenticate", `Basic realm="leak"`)
	w.Header().Set("Keep-Alive", "timeout=5")
	w.Header().Set("X-Upstream", "yes")
	switch r.URL.Path {
	case "/big":
		_, _ = w.Write(bytes.Repeat([]byte("x"), 5000))
		return
	case "/chunked":
		fl := w.(http.Flusher)
		for _, part := range []string{"alpha-", "beta-", "gamma"} {
			_, _ = io.WriteString(w, part)
			fl.Flush()
		}
		return
	case "/nocontent":
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fmt.Fprintf(w, "saw %s %s len=%d", r.Method, r.URL.String(), len(body))
}

func (f *fakeUpstream) upgrade(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.upgrades = append(f.upgrades, r.Header.Clone())
	f.mu.Unlock()
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: fake\r\n\r\n")
	_, _ = io.Copy(conn, rw.Reader) // echo frames
}

// echoServer is a TCP echo target. accepts counts connections, so a test can
// prove a target was (or was never) contacted.
type echoServer struct {
	ln      net.Listener
	accepts atomic.Int64
	mu      sync.Mutex
	conns   []net.Conn
	wg      sync.WaitGroup
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &echoServer{ln: ln}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			e.accepts.Add(1)
			e.mu.Lock()
			e.conns = append(e.conns, c)
			e.mu.Unlock()
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				_, _ = io.Copy(c, c)
				c.Close()
			}()
		}
	}()
	t.Cleanup(e.close)
	return e
}

func (e *echoServer) addr() string { return e.ln.Addr().String() }

func (e *echoServer) close() {
	_ = e.ln.Close()
	e.mu.Lock()
	for _, c := range e.conns {
		_ = c.Close()
	}
	e.mu.Unlock()
	e.wg.Wait()
}

// hangListener accepts connections and never answers.
func hangListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

// closedAddr is a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// testProxy bundles a started proxy with its metrics registry and log sink.
type testProxy struct {
	*Proxy
	reg  *prometheus.Registry
	logs *syncBuf
}

// startProxy starts a proxy with the allow-all guard, a private registry and a
// captured logger. mutate adjusts the config (it must set a Supplier).
func startProxy(t *testing.T, mutate func(*Config)) *testProxy {
	t.Helper()
	logs := &syncBuf{}
	reg := prometheus.NewRegistry()
	cfg := DefaultConfig()
	cfg.Registerer = reg
	cfg.Guard = allowAll
	cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg.DialTimeout = 3 * time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return &testProxy{Proxy: p, reg: reg, logs: logs}
}

// connectVia opens a CONNECT tunnel through the proxy and returns the
// connection, the response and the reader that holds any buffered bytes.
func connectVia(t *testing.T, proxyAddr, target string) (net.Conn, *http.Response, *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	return c, resp, br
}

// echoOnce sends msg through a tunnel and expects it back.
func echoOnce(t *testing.T, c net.Conn, br *bufio.Reader, msg string) {
	t.Helper()
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("echo = %q, want %q", buf, msg)
	}
}

// proxyClient is an http.Client whose requests go through the proxy.
func proxyClient(t *testing.T, p *Proxy) *http.Client {
	t.Helper()
	pu, err := url.Parse(p.URL())
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(pu)}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// metricValue reads one series from reg; a missing series is 0.
func metricValue(reg *prometheus.Registry, name string, labels map[string]string) float64 {
	mfs, err := reg.Gather()
	if err != nil {
		return -1
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			match := true
			for k, v := range labels {
				if got[k] != v {
					match = false
				}
			}
			if !match {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string, want float64) {
	t.Helper()
	eventually(t, 5*time.Second, fmt.Sprintf("%s%v = %v", name, labels, want), func() bool {
		return metricValue(reg, name, labels) == want
	})
}

func reqLabels(kind, result string) map[string]string {
	return map[string]string{"kind": kind, "result": result}
}

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

// hangListenerThatCloses accepts, reads the request head and closes without
// answering, which is a failure after the request was sent.
func hangListenerThatCloses(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = c.Read(make([]byte, 4096))
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	return ln.Addr().String()
}

func stringsReader(s string) io.Reader { return strings.NewReader(s) }

// connectViaE is connectVia for goroutines: it reports failure by a nil
// response instead of calling t.Fatal.
func connectViaE(proxyAddr, target string) (net.Conn, *http.Response, *bufio.Reader) {
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		return nil, nil, nil
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, nil, nil
	}
	return c, resp, br
}
