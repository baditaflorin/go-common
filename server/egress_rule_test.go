package server

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/egress"
)

// resetEgressRule gives a test its own once-per-process guard and install hook,
// and puts both back afterwards, so the rest of the package's tests still reach
// the real install the first time they call New.
func resetEgressRule(t *testing.T, hook func() egress.InstallStatus) {
	t.Helper()
	prevHook := installDefaultTransport
	installDefaultTransport = hook
	egressOnce = sync.Once{}
	t.Cleanup(func() {
		installDefaultTransport = prevHook
		egressOnce = sync.Once{}
	})
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// New is where every fleet service converges, so it is where the rule has to be
// applied: a service gets it with no code of its own.
func TestNewAppliesTheEgressRuleOncePerProcess(t *testing.T) {
	calls := 0
	resetEgressRule(t, func() egress.InstallStatus { calls++; return egress.DefaultInstalled })
	for i := 0; i < 3; i++ {
		_ = New(&config.Config{AppName: "egress_rule_wiring", Version: "0.0.0", Port: "0"})
	}
	if calls != 1 {
		t.Fatalf("New installed the egress rule %d times in three calls, want exactly once", calls)
	}
}

// The real call, end to end: after New the default transport follows the rule,
// which is what makes a plain client reach a sibling directly.
func TestNewPutsTheRuleOnTheDefaultTransport(t *testing.T) {
	t.Setenv(egress.DefaultTransportEnv, "") // a CI environment that switched it off must not fail this
	_ = New(&config.Config{AppName: "egress_rule_default", Version: "0.0.0", Port: "0"})
	if !egress.DefaultTransportInstalled() {
		t.Fatal("http.DefaultTransport does not follow the egress rule after server.New")
	}
}

func TestNewSaysWhatItDidWithTheEgressRule(t *testing.T) {
	cases := []struct {
		name   string
		status egress.InstallStatus
		want   []string
		absent []string
	}{
		{"installed", egress.DefaultInstalled, []string{"egress:", "directly", egress.DefaultTransportEnv}, nil},
		{"already installed", egress.DefaultAlready, nil, []string{"egress:"}},
		{"disabled", egress.DefaultDisabled, []string{"egress:", "unchanged", "disabled by " + egress.DefaultTransportEnv}, []string{"directly"}},
		{"custom proxy", egress.DefaultCustom, []string{"egress:", "unchanged", "own proxy function"}, []string{"directly"}},
		{"foreign transport", egress.DefaultForeign, []string{"egress:", "unchanged", "not an *http.Transport"}, []string{"directly"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetEgressRule(t, func() egress.InstallStatus { return tc.status })
			buf := captureLog(t)
			_ = New(&config.Config{AppName: "egress_rule_log", Version: "0.0.0", Port: "0"})
			out := buf.String()
			var lines []string
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, "server: egress:") {
					lines = append(lines, l)
				}
			}
			got := strings.Join(lines, "\n")
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("log %q does not contain %q", got, w)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("log %q contains %q", got, a)
				}
			}
			if tc.want == nil && got != "" {
				t.Errorf("an already-installed rule logged %q, want silence", got)
			}
		})
	}
}

// Nothing the rule logs may carry a value from the environment: only the name of
// the switch. A proxy URL can hold credentials.
func TestEgressRuleLogNeverPrintsProxyValues(t *testing.T) {
	const secretish = "http://user:pa55word@proxy.invalid:8080"
	t.Setenv("HTTP_PROXY", secretish)
	t.Setenv("HTTPS_PROXY", secretish)
	t.Setenv("NO_PROXY", "internal.invalid")
	resetEgressRule(t, func() egress.InstallStatus { return egress.DefaultInstalled })
	buf := captureLog(t)
	_ = New(&config.Config{AppName: "egress_rule_secret", Version: "0.0.0", Port: "0"})
	if strings.Contains(buf.String(), "pa55word") || strings.Contains(buf.String(), "proxy.invalid") {
		t.Fatalf("the log carries a proxy value: %q", buf.String())
	}
}
