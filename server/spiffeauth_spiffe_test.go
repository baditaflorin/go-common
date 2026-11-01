//go:build !gocommon_nospiffe

package server

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func capture() (func(string, ...any), *[]string) {
	var lines []string
	return func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }, &lines
}

// The inert default: nothing configured, nothing started, no log line.
func TestSPIFFEAuthIsInertByDefault(t *testing.T) {
	for _, v := range []string{"", "off"} {
		t.Setenv("SPIFFEAUTH_MODE", v)
		t.Setenv("SPIFFEAUTH_MTLS_ADDR", ":8443") // ignored while off
		logf, lines := capture()
		if rt := startSPIFFEAuth(nil, logf); rt != nil {
			t.Fatalf("mode %q started a runtime", v)
		}
		if len(*lines) != 0 {
			t.Fatalf("mode %q logged %v", v, *lines)
		}
	}
	s := &Server{}
	identityOnce.Do(func() {}) // keep the process-wide runtime out of this test
	if s.spiffeIdentity() != nil {
		t.Fatal("spiffeIdentity must be an untyped nil interface when off")
	}
}

// A broken configuration never stops a service: it is logged and the service runs on API keys.
func TestSPIFFEAuthBadConfigIsLoggedNotFatal(t *testing.T) {
	t.Setenv("SPIFFEAUTH_MODE", "dual")
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "")
	logf, lines := capture()
	if rt := startSPIFFEAuth(nil, logf); rt != nil {
		t.Fatal("a broken config started a runtime")
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "running with API keys only") {
		t.Fatalf("log = %v", *lines)
	}
}

// A valid configuration whose SPIRE is not up yet still starts the service: the handle
// connects in the background and authenticates nothing until it can.
func TestSPIFFEAuthDualStartsWithoutSPIRE(t *testing.T) {
	t.Setenv("SPIFFEAUTH_MODE", "dual")
	t.Setenv("SPIFFE_ENDPOINT_SOCKET", "unix://"+filepath.Join(t.TempDir(), "absent.sock"))
	t.Setenv("SPIFFEAUTH_PEERS", `{"peers":[{"id":"spiffe://0exec.com/ns/*/sa/agent-tools","principal":"agent-tools"}]}`)
	t.Setenv("SPIFFEAUTH_MTLS_ADDR", "127.0.0.1:0")
	logf, _ := capture()
	rt := startSPIFFEAuth(nil, logf)
	if rt == nil {
		t.Fatal("no runtime for a valid dual config")
	}
	defer rt.close()
	if rt.addr != "127.0.0.1:0" || rt.auth == nil || rt.tlsConfig() == nil {
		t.Fatalf("runtime = %+v", rt)
	}
	if p, ok := rt.auth.ExpectedPrincipal("agent-tools"); !ok || p != "agent-tools" {
		t.Fatal("peer map not loaded")
	}
}
