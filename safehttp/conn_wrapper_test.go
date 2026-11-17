package safehttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWithConnWrapperRunsAfterSafeGuardsAndWrapsProxySocket(t *testing.T) {
	SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1")})
	t.Cleanup(func() { SetAllowedPrivateIPs(nil) })
	var proxyHits, wraps atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		_, _ = io.WriteString(w, "routed")
	}))
	t.Cleanup(proxy.Close)
	client, err := NewClientE(
		WithoutFetchCache(),
		WithProxyURL(proxy.URL),
		WithConnWrapper(func(_ context.Context, conn net.Conn) (net.Conn, error) {
			wraps.Add(1)
			return conn, nil
		}),
	)
	if err != nil {
		t.Fatal("NewClientE:", err)
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://192.0.2.10/resource")
	if err != nil {
		t.Fatal("proxy request:", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "routed" || wraps.Load() != 1 || proxyHits.Load() != 1 {
		t.Fatalf("wrapped request result body=%q wraps=%d proxy_hits=%d err=%v", body, wraps.Load(), proxyHits.Load(), err)
	}
	if _, err := client.Get("http://127.0.0.2:8080/private"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "blocked") {
		t.Fatalf("private origin error = %v, want blocked", err)
	}
	if wraps.Load() != 1 || proxyHits.Load() != 1 {
		t.Fatalf("private origin reached wrapper/proxy: wraps=%d proxy_hits=%d", wraps.Load(), proxyHits.Load())
	}
}

func TestWithConnWrapperFailureClosesAndRejectsSocket(t *testing.T) {
	SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1")})
	t.Cleanup(func() { SetAllowedPrivateIPs(nil) })
	var hits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(proxy.Close)
	client, err := NewClientE(
		WithoutFetchCache(),
		WithProxyURL(proxy.URL),
		WithConnWrapper(func(_ context.Context, conn net.Conn) (net.Conn, error) {
			return nil, errors.New("ticket closed")
		}),
	)
	if err != nil {
		t.Fatal("NewClientE:", err)
	}
	defer client.CloseIdleConnections()
	if _, err := client.Get("http://192.0.2.10/resource"); err == nil || !strings.Contains(err.Error(), "ticket closed") {
		t.Fatalf("request error = %v, want wrapper error", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("proxy received request despite wrapper refusal: hits=%d", hits.Load())
	}
}
