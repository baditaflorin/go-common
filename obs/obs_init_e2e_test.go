package obs

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/promx"
)

// TestInit_ServesOnExplicitDebugAddr drives Init() (the opt-in one-liner
// services call in main()) end-to-end via DEBUG_ADDR and asserts the
// standalone debug server actually serves /debug/pprof/ + /metrics on
// localhost. obs is opt-in as of v0.62.0; this confirms the opt-in path
// works without server.New.
func TestInit_ServesOnExplicitDebugAddr(t *testing.T) {
	// The /metrics mirror reflects the shared promx registry; ensure the
	// Go runtime collectors are present so go_goroutines is exported.
	promx.Init("obs_e2e_test", "0.0.0")

	// Pick a free loopback port so the test is hermetic / parallel-safe.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	t.Setenv(EnvDisable, "")
	t.Setenv(EnvDebugAddr, addr)

	stop, err := Init()
	if err != nil {
		t.Fatalf("Init(): %v", err)
	}
	defer stop()

	base := "http://" + addr
	if code, _ := getBody(t, base+"/debug/pprof/"); code != http.StatusOK {
		t.Errorf("/debug/pprof/ status = %d, want 200", code)
	}
	code, body := getBody(t, base+"/metrics")
	if code != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", code)
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics missing go_goroutines")
	}
}

// TestInit_DebugAddrOffDoesNotBind confirms DEBUG_ADDR=off makes Init() a
// no-op that opens no listener.
func TestInit_DebugAddrOffDoesNotBind(t *testing.T) {
	// Reserve a port, free it, point DEBUG_ADDR at "off" (not the port);
	// then assert that the explicit-port path would have bound but "off"
	// does not. We assert via the disabled spelling directly.
	t.Setenv(EnvDisable, "")
	t.Setenv(EnvDebugAddr, "off")

	stop, err := Init()
	if err != nil {
		t.Fatalf("Init() with DEBUG_ADDR=off: %v", err)
	}
	defer stop()

	// DefaultDebugAddr must NOT be serving — "off" disables entirely.
	if conn, derr := net.DialTimeout("tcp", DefaultDebugAddr, 150*time.Millisecond); derr == nil {
		conn.Close()
		t.Fatalf("DEBUG_ADDR=off still left a listener on %s", DefaultDebugAddr)
	}
}
