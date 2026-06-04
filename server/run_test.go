package server

import (
	"testing"
	"time"

	"github.com/baditaflorin/go-common/config"
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
