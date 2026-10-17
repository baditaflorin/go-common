package obs

import (
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/promx"
	"github.com/baditaflorin/go-common/server"
)

// freeLoopbackAddr returns a loopback address nothing is listening on.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestDebugMetricsFollowsPromxInit: the debug server is started first, and
// promx.Init replaces the registry afterwards (a new service identity makes a
// new one). The /metrics mirror must serve the registry that exists when it is
// scraped, not the one that existed when the server started.
func TestDebugMetricsFollowsPromxInit(t *testing.T) {
	addr := freeLoopbackAddr(t)
	stop, err := StartDebugServer(addr)
	if err != nil {
		t.Fatalf("StartDebugServer: %v", err)
	}
	defer stop()

	promx.Init("obs_lazy_init", "1.2.3")

	code, body := getBody(t, "http://"+addr+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", code)
	}
	if !strings.Contains(body, `service="obs_lazy_init"`) || !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics does not serve the registry promx.Init created after the debug server started (%d bytes)", len(body))
	}
}

// TestInitBeforeServerNew is the order a service naturally has: obs.Init() at
// the top of main, server.New after it. The mirror must not stay empty.
func TestInitBeforeServerNew(t *testing.T) {
	addr := freeLoopbackAddr(t)
	t.Setenv(EnvDisable, "")
	t.Setenv(EnvDebugAddr, addr)
	stop, err := Init()
	if err != nil {
		t.Fatalf("Init(): %v", err)
	}
	defer stop()

	_ = server.New(&config.Config{AppName: "obs_lazy_server", Version: "0.0.1", Port: "0"})

	code, body := getBody(t, "http://"+addr+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", code)
	}
	for _, want := range []string{`service="obs_lazy_server"`, "go_goroutines", "http_requests_in_flight"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics mirror lacks %q after server.New ran (%d bytes)", want, len(body))
		}
	}
}
