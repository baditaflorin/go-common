package server_test

import (
	"net"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/server"
)

// TestNew_DoesNotStartDebugListener asserts that server.New is decoupled
// from the obs debug server: constructing a server must NOT open the
// localhost pprof/debug listener on the default obs port (127.0.0.1:6060).
//
// obs is opt-in as of v0.63.0 — a service that uses server.New but does
// not call obs.Init() must neither compile in net/http/pprof nor bind a
// debug listener. v0.61.0 briefly auto-started it from server.New; this
// guards against that regression.
func TestNew_DoesNotStartDebugListener(t *testing.T) {
	const obsAddr = "127.0.0.1:6060"

	// Sanity: the port must be free before we start, otherwise the test
	// is meaningless (something unrelated is already squatting it).
	probe, err := net.Listen("tcp", obsAddr)
	if err != nil {
		t.Skipf("%s already in use by an unrelated process; cannot assert", obsAddr)
	}
	_ = probe.Close()

	cfg := &config.Config{AppName: "test-svc", Version: "0.0.1", Port: "0"}
	_ = server.New(cfg)

	// Nothing should be listening on the obs port now: server.New must
	// not have auto-started the debug server.
	conn, err := net.DialTimeout("tcp", obsAddr, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("server.New started a debug listener on %s; obs must be opt-in (call obs.Init() explicitly)", obsAddr)
	}
}
