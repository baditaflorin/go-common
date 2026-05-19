package env_test

import (
	"os"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/env"
)

func TestString(t *testing.T) {
	t.Cleanup(env.SetEnv("TEST_STRING_KEY", "hello"))
	if got := env.String("TEST_STRING_KEY", "default"); got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestStringDefault(t *testing.T) {
	t.Cleanup(env.UnsetEnv("TEST_MISSING_KEY"))
	if got := env.String("TEST_MISSING_KEY", "fallback"); got != "fallback" {
		t.Fatalf("got %q, want %q", got, "fallback")
	}
}

func TestBool(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {"on", true},
		{"0", false}, {"false", false}, {"no", false}, {"off", false}, {"", false},
	}
	for _, c := range cases {
		os.Setenv("TEST_BOOL", c.val)
		defaultBool := !c.want // make sure we're testing the env, not the default
		if c.val == "" {
			defaultBool = false
		}
		got := env.Bool("TEST_BOOL", defaultBool)
		if c.val == "" {
			if got != defaultBool {
				t.Fatalf("empty val: got %v, want default %v", got, defaultBool)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("val=%q: got %v, want %v", c.val, got, c.want)
		}
	}
	os.Unsetenv("TEST_BOOL")
}

func TestInt(t *testing.T) {
	t.Cleanup(env.SetEnv("TEST_INT", "42"))
	if got := env.Int("TEST_INT", 0); got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
}

func TestIntInvalid(t *testing.T) {
	t.Cleanup(env.SetEnv("TEST_INT_BAD", "notanint"))
	if got := env.Int("TEST_INT_BAD", 99); got != 99 {
		t.Fatalf("got %d, want default 99", got)
	}
}

func TestFloat64(t *testing.T) {
	t.Cleanup(env.SetEnv("TEST_FLOAT", "3.14"))
	if got := env.Float64("TEST_FLOAT", 0); got != 3.14 {
		t.Fatalf("got %v, want 3.14", got)
	}
}

func TestDuration(t *testing.T) {
	t.Cleanup(env.SetEnv("TEST_DUR", "250ms"))
	if got := env.Duration("TEST_DUR", 0); got != 250*time.Millisecond {
		t.Fatalf("got %v, want 250ms", got)
	}
}

func TestDurationDefault(t *testing.T) {
	t.Cleanup(env.UnsetEnv("TEST_DUR_MISSING"))
	want := 5 * time.Second
	if got := env.Duration("TEST_DUR_MISSING", want); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestStrings(t *testing.T) {
	t.Cleanup(env.SetEnv("TEST_LIST", "a, b , c"))
	got := env.Strings("TEST_LIST", ",", nil)
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("got %v", got)
	}
}

func TestStringsDefault(t *testing.T) {
	t.Cleanup(env.UnsetEnv("TEST_LIST_MISSING"))
	def := []string{"x", "y"}
	got := env.Strings("TEST_LIST_MISSING", ",", def)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestSetEnvRestores(t *testing.T) {
	os.Setenv("TEST_RESTORE", "original")
	restore := env.SetEnv("TEST_RESTORE", "changed")
	if os.Getenv("TEST_RESTORE") != "changed" {
		t.Fatal("expected changed")
	}
	restore()
	if os.Getenv("TEST_RESTORE") != "original" {
		t.Fatal("expected original restored")
	}
	os.Unsetenv("TEST_RESTORE")
}

func TestRequirePanics(t *testing.T) {
	os.Unsetenv("TEST_REQUIRE_MISSING")
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from Require with missing var")
		}
	}()
	env.Require("TEST_REQUIRE_MISSING")
}

func TestRequireOK(t *testing.T) {
	t.Cleanup(env.SetEnv("TEST_REQUIRE_SET", "val"))
	// should not panic
	env.Require("TEST_REQUIRE_SET")
}
