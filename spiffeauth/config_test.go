package spiffeauth

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/middleware"
)

func envMap(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

func TestConfigOffByDefaultWhateverElseIsSet(t *testing.T) {
	for _, mode := range []string{"", "off", "OFF", "false", "0", "disabled"} {
		cfg, err := configFrom(envMap(map[string]string{EnvMode: mode, EnvSocket: "garbage", EnvPeers: "not json", EnvJWTMaxTTL: "x"}))
		if err != nil || cfg.Enabled() || cfg.Mode != ModeOff {
			t.Fatalf("mode %q: cfg=%+v err=%v", mode, cfg, err)
		}
	}
	h, err := Start(Config{}, nil)
	if h != nil || err != nil {
		t.Fatalf("Start(zero Config) = %v, %v; the inert default must start nothing", h, err)
	}
}

func TestConfigDualFromEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "peers.json")
	_ = os.WriteFile(file, []byte(`{"peers":[{"id":"spiffe://0exec.com/ns/*/sa/agent-tools","principal":"agent-tools","tier":"internal"}]}`), 0o600)
	cfg, err := configFrom(envMap(map[string]string{
		EnvMode: "dual", EnvSocket: "unix:///run/spire/agent.sock", EnvPeersFile: file,
		EnvJWTAudience: "a, b", EnvJWTMaxTTL: "5m", EnvMTLSAddr: ":8443", EnvUnmapped: "fallback",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled() || len(cfg.Peers) != 1 || cfg.JWTMaxTTL != 5*time.Minute || cfg.MTLSAddr != ":8443" ||
		cfg.Unmapped != UnmappedFallback || len(cfg.JWTAudience) != 2 || cfg.TrustDomains[0] != "0exec.com" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestConfigDualRejectsWhatCannotWork(t *testing.T) {
	good := map[string]string{EnvMode: "dual", EnvSocket: "unix:///run/spire/agent.sock",
		EnvPeers: `{"peers":[{"id":"spiffe://0exec.com/ns/fleet/sa/a-b","principal":"a-b"}]}`}
	if _, err := configFrom(envMap(good)); err != nil {
		t.Fatal(err)
	}
	mut := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range good {
			m[a] = b
		}
		m[k] = v
		return m
	}
	bad := map[string]map[string]string{
		"mode typo":         mut(EnvMode, "enforce"),
		"no socket":         mut(EnvSocket, ""),
		"tcp socket":        mut(EnvSocket, "tcp://127.0.0.1:8081"),
		"no peers":          mut(EnvPeers, ""),
		"unknown field":     mut(EnvPeers, `{"peers":[{"id":"spiffe://0exec.com/ns/fleet/sa/a-b","principal":"a-b","teir":"x"}]}`),
		"ttl too long":      mut(EnvJWTMaxTTL, "1h"),
		"ttl too short":     mut(EnvJWTMaxTTL, "1s"),
		"unmapped typo":     mut(EnvUnmapped, "allow"),
		"missing file":      mut(EnvPeersFile, "/nonexistent/peers.json"),
		"foreign domain":    mut(EnvPeers, `{"peers":[{"id":"spiffe://evil.example/ns/fleet/sa/a-b","principal":"a-b"}]}`),
		"bad principal":     mut(EnvPeers, `{"peers":[{"id":"spiffe://0exec.com/ns/fleet/sa/a-b","principal":"A B"}]}`),
		"id not workload":   mut(EnvPeers, `{"peers":[{"id":"spiffe://0exec.com/service/x","principal":"a-b"}]}`),
		"duplicate":         mut(EnvPeers, `{"peers":[{"id":"spiffe://0exec.com/ns/fleet/sa/a-b","principal":"a-b"},{"id":"spiffe://0exec.com/ns/fleet/sa/a-b","principal":"c-d"}]}`),
		"wildcard twice":    mut(EnvPeers, `{"peers":[{"id":"spiffe://0exec.com/ns/*/sa/a-b","principal":"a-b"},{"id":"spiffe://0exec.com/ns/*/sa/a-b","principal":"c-d"}]}`),
		"wildcard sa":       mut(EnvPeers, `{"peers":[{"id":"spiffe://0exec.com/ns/fleet/sa/*","principal":"a-b"}]}`),
		"trailing slash id": mut(EnvPeers, `{"peers":[{"id":"spiffe://0exec.com/ns/fleet/sa/a-b/","principal":"a-b"}]}`),
	}
	for name, m := range bad {
		if _, err := configFrom(envMap(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseWorkloadID(t *testing.T) {
	for id, want := range map[string][3]string{
		"spiffe://0exec.com/ns/fleet/sa/agent-tools": {"0exec.com", "fleet", "agent-tools"},
		"spiffe://0exec.com/ns/0crawl/sa/go-x":       {"0exec.com", "0crawl", "go-x"},
	} {
		td, ns, sa, ok := ParseWorkloadID(id)
		if !ok || [3]string{td, ns, sa} != want {
			t.Errorf("%s -> %v %v %v %v", id, td, ns, sa, ok)
		}
	}
	for _, id := range []string{"", "https://x/ns/a/sa/b", "spiffe://0exec.com", "spiffe://0exec.com/ns/fleet", "spiffe://0exec.com/ns/fleet/sa/", "spiffe://0exec.com/x/fleet/sa/b", "spiffe://0exec.com/ns/fleet/sa/b/c", "spiffe:///ns/a/sa/b"} {
		if _, _, _, ok := ParseWorkloadID(id); ok {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestWildcardCoversOnlyTheTwoFleetNamespaces(t *testing.T) {
	pm, err := newPeerMap([]Peer{{ID: "spiffe://0exec.com/ns/*/sa/svc-a", Principal: "svc-a"}, {ID: "spiffe://0exec.com/ns/fleet/sa/svc-b", Principal: "svc-b"}}, []string{"0exec.com"})
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeSPIRE(t, "0exec.com")
	look := func(id string) bool { _, ok := pm.lookup(f.svid(id).ID); return ok }
	if !look("spiffe://0exec.com/ns/fleet/sa/svc-a") || !look("spiffe://0exec.com/ns/0crawl/sa/svc-a") {
		t.Fatal("wildcard must cover fleet and 0crawl")
	}
	if look("spiffe://0exec.com/ns/evil/sa/svc-a") || look("spiffe://0exec.com/ns/0crawl/sa/svc-b") || look("spiffe://0exec.com/ns/fleet/sa/svc-c") {
		t.Fatal("matched where it must not")
	}
}

// Start must never block service start-up and must refuse to authenticate while SPIRE is not reachable.
func TestHandleBeforeTheWorkloadAPIIsUp(t *testing.T) {
	rec := &recorder{}
	cfg := testConfig(peers()...)
	cfg.Socket = "unix://" + filepath.Join(t.TempDir(), "absent.sock")
	cfg.ConnectTimeout = 200 * time.Millisecond
	cfg.Observer = rec
	var logged []string
	h, err := Start(cfg, func(f string, a ...any) { logged = append(logged, f) })
	if err != nil || h == nil {
		t.Fatalf("Start = %v, %v", h, err)
	}
	defer h.Close()
	if h.Ready() {
		t.Fatal("ready without a Workload API")
	}
	r := mustReq(t)
	if d := h.AuthenticateIdentity(r); d.Presented || d.Allowed || d.Deny {
		t.Fatalf("decision = %+v; until ready every request must go the API key path", d)
	}
	if p, ok := h.ExpectedPrincipal("agent-tools"); !ok || p != "agent-tools" {
		t.Fatal("ExpectedPrincipal works from the config alone")
	}
	cc, err := h.TLSConfig().GetConfigForClient(&tls.ClientHelloInfo{})
	if cc != nil || err == nil {
		t.Fatal("handshakes must be refused until ready")
	}
	_, failures, _ := rec.snapshot()
	if len(failures) != 1 || failures[0] != "spiffe-mtls/not_ready" {
		t.Fatalf("failures = %v", failures)
	}
	done := make(chan struct{})
	go func() { _ = h.Close(); _ = h.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked")
	}
	for _, l := range logged {
		if strings.Contains(l, "ak_") {
			t.Fatal("log line carries a credential")
		}
	}
}

func TestNewRefusesADisabledOrBrokenConfig(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("New must refuse a disabled config")
	}
	cfg := testConfig(peers()...)
	cfg.Socket = "tcp://127.0.0.1:1"
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("a remote socket must be refused")
	}
	var _ middleware.IdentityAuthenticator = (*Handle)(nil)
}

func TestConfigStrictPresentedFromEnv(t *testing.T) {
	base := map[string]string{EnvMode: "dual", EnvSocket: "unix:///run/spire/agent.sock",
		EnvPeers: `{"peers":[{"id":"spiffe://0exec.com/ns/fleet/sa/a-b","principal":"a-b"}]}`}
	cfg, err := configFrom(envMap(base))
	if err != nil || cfg.StrictPresented {
		t.Fatalf("default must be off: cfg=%+v err=%v", cfg, err)
	}
	base[EnvStrictPresented] = "true"
	cfg, err = configFrom(envMap(base))
	if err != nil || !cfg.StrictPresented {
		t.Fatalf("SPIFFEAUTH_STRICT_PRESENTED=true must turn it on: cfg=%+v err=%v", cfg, err)
	}
	var h *Handle
	if h.StrictPresented() {
		t.Fatal("a nil Handle is never strict")
	}
}
