package obs

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/promx"
	"github.com/prometheus/client_golang/prometheus"
)

// getBody is a small helper: GET url and return (status, body).
func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestDebugServer_FixedPort exercises the real endpoints on a known
// loopback address.
func TestDebugServer_FixedPort(t *testing.T) {
	promx.Init("obs_test", "0.0.0")

	const addr = "127.0.0.1:16099"
	stop, err := StartDebugServer(addr)
	if err != nil {
		t.Fatalf("StartDebugServer(%s): %v", addr, err)
	}
	defer stop()

	base := "http://" + addr

	// /debug/pprof/ index must serve 200.
	if code, _ := getBody(t, base+"/debug/pprof/"); code != http.StatusOK {
		t.Errorf("/debug/pprof/ status = %d, want 200", code)
	}

	// /metrics must serve 200 and contain a Go runtime metric.
	code, body := getBody(t, base+"/metrics")
	if code != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", code)
	}
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics body missing go_goroutines; got %d bytes", len(body))
	}
}

func TestStartDebugServer_DisabledWhenAddrEmpty(t *testing.T) {
	stop, err := StartDebugServer("")
	if err != nil {
		t.Fatalf("StartDebugServer(\"\"): unexpected err %v", err)
	}
	if stop == nil {
		t.Fatal("StartDebugServer(\"\") returned nil StopFunc")
	}
	// Must be safe to call (no panic) even though nothing was started.
	stop()
	stop() // idempotent
}

func TestStartDebugServer_BindErrorReturned(t *testing.T) {
	// 1 is a privileged/invalid port for an unprivileged test process;
	// binding it must surface an error, not silently swallow it.
	if _, err := StartDebugServer("127.0.0.1:1"); err == nil {
		t.Skip("binding 127.0.0.1:1 unexpectedly succeeded (running as root?)")
	}
}

func TestResolveAddr(t *testing.T) {
	cases := []struct {
		name      string
		debugAddr string
		disable   string
		want      string
	}{
		{"default", "", "", DefaultDebugAddr},
		{"explicit", "127.0.0.1:7777", "", "127.0.0.1:7777"},
		{"off", "off", "", ""},
		{"disabled", "disabled", "", ""},
		{"zero", "0", "", ""},
		{"obs_disable", "127.0.0.1:7777", "1", ""},
		{"obs_disable_false", "127.0.0.1:7777", "false", "127.0.0.1:7777"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDebugAddr, tc.debugAddr)
			t.Setenv(EnvDisable, tc.disable)
			if got := resolveAddr(); got != tc.want {
				t.Errorf("resolveAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInit_DisabledIsNoop(t *testing.T) {
	t.Setenv(EnvDisable, "1")
	stop, err := Init()
	if err != nil {
		t.Fatalf("Init disabled: %v", err)
	}
	stop() // no-op, must not panic
}

func TestRegisterRuntimeMetrics_Idempotent(t *testing.T) {
	reg := prometheus.NewRegistry()

	if err := RegisterRuntimeMetrics(reg); err != nil {
		t.Fatalf("first RegisterRuntimeMetrics: %v", err)
	}
	// Second call on the same registry must not error (AlreadyRegistered
	// is swallowed) and must not panic.
	if err := RegisterRuntimeMetrics(reg); err != nil {
		t.Fatalf("second RegisterRuntimeMetrics: %v", err)
	}

	// The collectors must actually be present.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var sawGoroutines bool
	for _, mf := range mfs {
		if mf.GetName() == "go_goroutines" {
			sawGoroutines = true
		}
	}
	if !sawGoroutines {
		t.Error("go_goroutines not registered after RegisterRuntimeMetrics")
	}
}

func TestRegisterRuntimeMetrics_NilReg(t *testing.T) {
	if err := RegisterRuntimeMetrics(nil); err != nil {
		t.Fatalf("nil registry should be a no-op, got %v", err)
	}
}
