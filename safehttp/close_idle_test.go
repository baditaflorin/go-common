package safehttp

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/telemetry"
)

type idleCloseRecorder struct{ closed atomic.Int64 }

func (t *idleCloseRecorder) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
func (t *idleCloseRecorder) CloseIdleConnections()                           { t.closed.Add(1) }

func TestCloseIdleConnectionsForwardsThroughEveryDecorator(t *testing.T) {
	base := &idleCloseRecorder{}
	client := &http.Client{Transport: &egressAllowlistTransport{inner: &uaTransport{inner: &extrasTransport{inner: telemetry.NewTransport(graph.RoundTripper(base))}}}}
	client.CloseIdleConnections()
	if got := base.closed.Load(); got != 1 {
		t.Fatalf("inner close calls=%d, want1", got)
	}
}

func TestCloseIdleConnectionsClosesBothTLSFallbackPools(t *testing.T) {
	closed := make(chan struct{}, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()
	primary, fallback := &http.Transport{}, &http.Transport{}
	defer primary.CloseIdleConnections()
	defer fallback.CloseIdleConnections()
	for _, transport := range []*http.Transport{primary, fallback} {
		resp, err := (&http.Client{Transport: transport}).Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	client := &http.Client{Transport: &extrasTransport{inner: telemetry.NewTransport(graph.RoundTripper(&tls12FallbackTransport{primary: primary, fallback: fallback}))}}
	client.CloseIdleConnections()
	for range 2 {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("idle connection pool stayed open")
		}
	}
}

type idleNonClosingTransport struct{}

func (idleNonClosingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

func TestCloseIdleConnectionsSupportsBaseWithoutOptionalInterface(t *testing.T) {
	client := &http.Client{Transport: &egressAllowlistTransport{inner: &uaTransport{inner: &extrasTransport{inner: telemetry.NewTransport(graph.RoundTripper(idleNonClosingTransport{}))}}}}
	client.CloseIdleConnections()
}

func BenchmarkCloseIdleDecoratorForwarding(b *testing.B) {
	base := &idleCloseRecorder{}
	client := &http.Client{Transport: &egressAllowlistTransport{inner: &uaTransport{inner: &extrasTransport{inner: telemetry.NewTransport(graph.RoundTripper(base))}}}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		client.CloseIdleConnections()
	}
}
