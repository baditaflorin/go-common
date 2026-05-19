// Package env provides typed, zero-dependency helpers for reading
// environment variables with defaults, required-or-fatal semantics,
// and common Go types (bool, int, duration, slice).
//
// Every fleet service needs at least a dozen env knobs. Without a
// shared layer each service re-implements os.Getenv + strconv +
// log.Fatal, each subtly differently. This package collapses that
// into one place with consistent error messages and a testable
// override path (SetEnv / UnsetEnv in tests).
//
// Usage:
//
//	port     := env.String("PORT", "8080")
//	enabled  := env.Bool("GRAPH_ENABLED", true)
//	timeout  := env.Duration("SELFTEST_TIMEOUT", 5*time.Second)
//	workers  := env.Int("WORKER_COUNT", 4)
//	url      := env.MustString("APIKEY_SERVICE_URL") // fatal if unset
package env

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// String returns the value of the named environment variable, or
// defaultVal if it is unset or empty.
func String(name, defaultVal string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return defaultVal
}

// MustString returns the value of the named environment variable.
// It calls log.Fatalf and exits 1 if the variable is unset or empty,
// printing a copy-paste remediation hint.
func MustString(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("env: required variable %q is not set\n"+
			"  hint: export %s=<value>", name, name)
	}
	return v
}

// Bool returns the boolean value of the named environment variable,
// or defaultVal if it is unset or empty. Accepted truthy values:
// "1", "true", "yes", "on" (case-insensitive). Everything else is false.
func Bool(name string, defaultVal bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return defaultVal
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Int returns the integer value of the named environment variable,
// or defaultVal if it is unset, empty, or not a valid integer.
// Parse errors are logged as warnings but do not crash the process.
func Int(name string, defaultVal int) int {
	v := os.Getenv(name)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("env: %s=%q is not a valid integer, using default %d: %v",
			name, v, defaultVal, err)
		return defaultVal
	}
	return n
}

// MustInt returns the integer value of the named environment variable.
// Exits 1 if the variable is unset, empty, or not a valid integer.
func MustInt(name string) int {
	v := MustString(name)
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("env: %s=%q is not a valid integer: %v", name, v, err)
	}
	return n
}

// Int64 returns the int64 value of the named environment variable,
// or defaultVal if it is unset, empty, or not a valid integer.
func Int64(name string, defaultVal int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Printf("env: %s=%q is not a valid int64, using default %d: %v",
			name, v, defaultVal, err)
		return defaultVal
	}
	return n
}

// Float64 returns the float64 value of the named environment variable,
// or defaultVal if it is unset, empty, or not a valid float.
func Float64(name string, defaultVal float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return defaultVal
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("env: %s=%q is not a valid float64, using default %g: %v",
			name, v, defaultVal, err)
		return defaultVal
	}
	return f
}

// Duration returns the time.Duration value of the named environment
// variable, or defaultVal if it is unset, empty, or not parseable by
// time.ParseDuration. Example values: "5s", "100ms", "2m30s".
func Duration(name string, defaultVal time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("env: %s=%q is not a valid duration, using default %s: %v",
			name, v, defaultVal, err)
		return defaultVal
	}
	return d
}

// MustDuration returns the time.Duration value of the named environment
// variable. Exits 1 if unset or not parseable.
func MustDuration(name string) time.Duration {
	v := MustString(name)
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("env: %s=%q is not a valid duration: %v", name, v, err)
	}
	return d
}

// Strings returns a []string by splitting the named environment variable
// on sep. If the variable is unset or empty, defaultVal is returned.
// Whitespace is trimmed from each element; empty elements are dropped.
//
//	# e.g. ALLOWED_ORIGINS=https://a.com,https://b.com
//	origins := env.Strings("ALLOWED_ORIGINS", ",", nil)
func Strings(name, sep string, defaultVal []string) []string {
	v := os.Getenv(name)
	if v == "" {
		return defaultVal
	}
	parts := strings.Split(v, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return defaultVal
	}
	return out
}

// Require panics (not log.Fatal) with a human-readable message when
// any of the listed env vars is unset or empty. Intended for test
// helpers and integration test setup where panic is more appropriate
// than os.Exit.
//
//	env.Require("APIKEY_SERVICE_URL", "GRAPH_COLLECTOR_URL")
func Require(names ...string) {
	var missing []string
	for _, name := range names {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		panic(fmt.Sprintf("env.Require: missing required env vars: %s", strings.Join(missing, ", ")))
	}
}

// SetEnv sets name=value for the duration of a test and returns a
// cleanup function that restores the previous value (or unsets it).
// Intended for use in t.Cleanup():
//
//	t.Cleanup(env.SetEnv("PORT", "9090"))
func SetEnv(name, value string) func() {
	prev, had := os.LookupEnv(name)
	os.Setenv(name, value)
	return func() {
		if had {
			os.Setenv(name, prev)
		} else {
			os.Unsetenv(name)
		}
	}
}

// UnsetEnv clears name and returns a cleanup function that restores it.
func UnsetEnv(name string) func() {
	prev, had := os.LookupEnv(name)
	os.Unsetenv(name)
	return func() {
		if had {
			os.Setenv(name, prev)
		}
	}
}
