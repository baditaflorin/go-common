package obs

import (
	"net/http"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/env"
	"github.com/baditaflorin/go-common/promx"
)

// The debug server lists the declared environment variables, with credentials
// redacted: an operator on the host can see how a process is configured without
// being able to read its secrets.
func TestDebugServer_ServesRedactedConfig(t *testing.T) {
	promx.Init("obs_config_test", "0.0.0")
	env.Declare(env.Var{Name: "OBS_TEST_SECRET", Kind: env.KindString, Secret: true, Doc: "a credential"})
	env.Declare(env.Var{Name: "OBS_TEST_LEVEL", Kind: env.KindString, Default: "info", Doc: "log level"})
	t.Setenv("OBS_TEST_SECRET", "do-not-print-this-value")
	t.Setenv("OBS_TEST_LEVEL", "debug")

	const addr = "127.0.0.1:16098"
	stop, err := StartDebugServer(addr)
	if err != nil {
		t.Fatalf("StartDebugServer(%s): %v", addr, err)
	}
	defer stop()

	code, body := getBody(t, "http://"+addr+"/debug/config")
	if code != http.StatusOK {
		t.Fatalf("/debug/config status = %d, want 200", code)
	}
	if strings.Contains(body, "do-not-print-this-value") {
		t.Fatalf("/debug/config leaked a secret: %s", body)
	}
	for _, want := range []string{`"OBS_TEST_SECRET"`, `"[redacted]"`, `"OBS_TEST_LEVEL"`, `"debug"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/debug/config body is missing %s", want)
		}
	}
}
