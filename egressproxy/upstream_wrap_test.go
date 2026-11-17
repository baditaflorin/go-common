package egressproxy

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestDialUpstreamWrapsSocketBeforeProxyHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen:", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	var wraps atomic.Int64
	p := &Proxy{cfg: Config{WrapUpstreamConn: func(conn net.Conn) (net.Conn, error) {
		wraps.Add(1)
		return conn, nil
	}}}
	conn, err := p.dialUpstream(context.Background(), &upstream{addr: ln.Addr().String()})
	if err != nil {
		t.Fatal("dialUpstream:", err)
	}
	_ = conn.Close()
	if wraps.Load() != 1 {
		t.Fatalf("wrapper calls = %d, want 1", wraps.Load())
	}
	select {
	case peer := <-accepted:
		_ = peer.Close()
	case <-time.After(time.Second):
		t.Fatal("upstream socket was not accepted")
	}
}

func TestDialUpstreamWrapperErrorClosesRawSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen:", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	wrapErr := errors.New("budget ticket closed")
	var raw net.Conn
	p := &Proxy{cfg: Config{WrapUpstreamConn: func(conn net.Conn) (net.Conn, error) {
		raw = conn
		return nil, wrapErr
	}}}
	if conn, err := p.dialUpstream(context.Background(), &upstream{addr: ln.Addr().String()}); conn != nil || !errors.Is(err, wrapErr) {
		t.Fatalf("dialUpstream = (%v,%v), want nil and wrapper error", conn, err)
	}
	if raw == nil {
		t.Fatal("wrapper did not receive the raw socket")
	}
	_ = raw.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := raw.Write([]byte("must be closed")); err == nil {
		t.Fatal("raw socket remained writable after wrapper failure")
	}
	select {
	case peer := <-accepted:
		_ = peer.Close()
	case <-time.After(time.Second):
		t.Fatal("upstream socket was not accepted")
	}
}
