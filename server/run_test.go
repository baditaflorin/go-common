package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/env"
)

func TestWithWriteTimeout(t *testing.T) {
	cfg := &config.Config{AppName: "go_wt_test", Version: "0.0.0", Port: "0"}

	// Default: no option → writeTimeout field stays 0, Start uses
	// DefaultWriteTimeout.
	if got := New(cfg).writeTimeout; got != 0 {
		t.Fatalf("default writeTimeout: got %v, want 0 (= DefaultWriteTimeout at Start)", got)
	}

	// Override applies.
	if got := New(cfg, WithWriteTimeout(65*time.Second)).writeTimeout; got != 65*time.Second {
		t.Fatalf("WithWriteTimeout(65s): got %v, want 65s", got)
	}

	// Non-positive durations are ignored (keep the default).
	if got := New(cfg, WithWriteTimeout(0)).writeTimeout; got != 0 {
		t.Fatalf("WithWriteTimeout(0): got %v, want 0 (ignored)", got)
	}
	if got := New(cfg, WithWriteTimeout(-5*time.Second)).writeTimeout; got != 0 {
		t.Fatalf("WithWriteTimeout(-5s): got %v, want 0 (ignored)", got)
	}
}

func TestWithReadTimeout(t *testing.T) {
	cfg := &config.Config{AppName: "go_rt_test", Version: "0.0.0", Port: "0"}

	if got := New(cfg).readTimeout; got != 0 {
		t.Fatalf("default readTimeout: got %v, want 0 (= DefaultReadTimeout at Start)", got)
	}
	if got := New(cfg, WithReadTimeout(20*time.Second)).readTimeout; got != 20*time.Second {
		t.Fatalf("WithReadTimeout(20s): got %v, want 20s", got)
	}
	// Non-positive durations are ignored (keep the default).
	if got := New(cfg, WithReadTimeout(0)).readTimeout; got != 0 {
		t.Fatalf("WithReadTimeout(0): got %v, want 0 (ignored)", got)
	}
	if got := New(cfg, WithReadTimeout(-1*time.Second)).readTimeout; got != 0 {
		t.Fatalf("WithReadTimeout(-1s): got %v, want 0 (ignored)", got)
	}
}

func TestWithServerTimeouts(t *testing.T) {
	cfg := &config.Config{AppName: "go_st_test", Version: "0.0.0", Port: "0"}

	// All three set at once.
	s := New(cfg, WithServerTimeouts(15*time.Second, 70*time.Second, 200*time.Second))
	if s.readTimeout != 15*time.Second || s.writeTimeout != 70*time.Second || s.idleTimeout != 200*time.Second {
		t.Fatalf("WithServerTimeouts: got read=%v write=%v idle=%v; want 15s/70s/200s",
			s.readTimeout, s.writeTimeout, s.idleTimeout)
	}

	// A 0 argument leaves that field at the default (unset = 0).
	s = New(cfg, WithServerTimeouts(0, 70*time.Second, 0))
	if s.readTimeout != 0 || s.writeTimeout != 70*time.Second || s.idleTimeout != 0 {
		t.Fatalf("WithServerTimeouts(0,70s,0): got read=%v write=%v idle=%v; want 0/70s/0",
			s.readTimeout, s.writeTimeout, s.idleTimeout)
	}
}

// TestBuildHTTPServerTimeouts proves the option values actually flow
// through to the *http.Server that Start() serves with — not just into
// the Server config fields. buildHTTPServer is the exact constructor
// Start() calls.
func TestBuildHTTPServerTimeouts(t *testing.T) {
	cfg := &config.Config{AppName: "go_http_test", Version: "0.0.0", Port: "0"}

	// No overrides → all three Default* values.
	def := New(cfg).buildHTTPServer(":0", http.NotFoundHandler())
	if def.ReadTimeout != DefaultReadTimeout ||
		def.WriteTimeout != DefaultWriteTimeout ||
		def.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("defaults: got read=%v write=%v idle=%v; want %v/%v/%v",
			def.ReadTimeout, def.WriteTimeout, def.IdleTimeout,
			DefaultReadTimeout, DefaultWriteTimeout, DefaultIdleTimeout)
	}

	// The RenderJS case from the task: raise WriteTimeout to 70s so a
	// cold ~60s render leg can return a 200 instead of a 30s-cutoff 502.
	wt := New(cfg, WithWriteTimeout(70*time.Second)).buildHTTPServer(":0", http.NotFoundHandler())
	if wt.WriteTimeout != 70*time.Second {
		t.Fatalf("WithWriteTimeout(70s): http.Server.WriteTimeout = %v, want 70s", wt.WriteTimeout)
	}
	if wt.ReadTimeout != DefaultReadTimeout || wt.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("WithWriteTimeout should not touch read/idle: got read=%v idle=%v", wt.ReadTimeout, wt.IdleTimeout)
	}

	// Trio override flows through to all three http.Server fields.
	all := New(cfg, WithServerTimeouts(15*time.Second, 70*time.Second, 200*time.Second)).
		buildHTTPServer(":0", http.NotFoundHandler())
	if all.ReadTimeout != 15*time.Second || all.WriteTimeout != 70*time.Second || all.IdleTimeout != 200*time.Second {
		t.Fatalf("WithServerTimeouts → http.Server: got read=%v write=%v idle=%v; want 15s/70s/200s",
			all.ReadTimeout, all.WriteTimeout, all.IdleTimeout)
	}
}

// TestServerTimeoutEnvOverride covers the SERVER_*_TIMEOUT_SECONDS env
// knobs — the fix for bare server.Run services (e.g. the RenderJS leaf
// detectors) that can't easily pass an Option but still need the 30 s
// write cap lifted. Precedence: option > env > default.
func TestServerTimeoutEnvOverride(t *testing.T) {
	cfg := &config.Config{AppName: "go_env_test", Version: "0.0.0", Port: "0"}

	// Env raises the write deadline when no option is set.
	defer env.SetEnv("SERVER_WRITE_TIMEOUT_SECONDS", "70")()
	srv := New(cfg).buildHTTPServer(":0", http.NotFoundHandler())
	if srv.WriteTimeout != 70*time.Second {
		t.Fatalf("SERVER_WRITE_TIMEOUT_SECONDS=70: WriteTimeout = %v, want 70s", srv.WriteTimeout)
	}
	// Read + idle untouched (no env, no option) → defaults.
	if srv.ReadTimeout != DefaultReadTimeout || srv.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("only write env set: got read=%v idle=%v; want %v/%v",
			srv.ReadTimeout, srv.IdleTimeout, DefaultReadTimeout, DefaultIdleTimeout)
	}

	// An explicit option wins over the env knob (deliberate code intent).
	srv = New(cfg, WithWriteTimeout(90*time.Second)).buildHTTPServer(":0", http.NotFoundHandler())
	if srv.WriteTimeout != 90*time.Second {
		t.Fatalf("option should beat env: WriteTimeout = %v, want 90s", srv.WriteTimeout)
	}

	// A non-positive / garbage env value is ignored → default.
	defer env.SetEnv("SERVER_READ_TIMEOUT_SECONDS", "0")()
	defer env.SetEnv("SERVER_IDLE_TIMEOUT_SECONDS", "not-a-number")()
	srv = New(cfg).buildHTTPServer(":0", http.NotFoundHandler())
	if srv.ReadTimeout != DefaultReadTimeout {
		t.Fatalf("SERVER_READ_TIMEOUT_SECONDS=0 ignored: got %v, want %v", srv.ReadTimeout, DefaultReadTimeout)
	}
	if srv.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("garbage idle env ignored: got %v, want %v", srv.IdleTimeout, DefaultIdleTimeout)
	}
}

func TestKebabAlias(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"go_email_extractor", "email-extractor"},
		{"go_ad_density", "ad-density"},
		{"go_a11y_quick", "a11y-quick"},
		{"go_xss_scanner", "xss-scanner"},
		{"go_scc", "scc"},     // single-word stays single-word
		{"plain", ""},         // no go_ prefix, no underscores → no alias
		{"already-kebab", ""}, // no go_ prefix, no underscores → no alias
		{"snake_case_no_go_prefix", "snake-case-no-go-prefix"},
	}
	for _, c := range cases {
		got := KebabAlias(c.in)
		if got != c.want {
			t.Errorf("KebabAlias(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
