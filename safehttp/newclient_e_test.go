package safehttp_test

import (
	"fmt"
	"testing"

	"github.com/baditaflorin/go-common/safehttp"
)

// The panic texts below are what NewClient has always panicked with. They are
// spelled out here, and not taken from NewClientE, so that a changed text fails
// the test: log search and runbooks match on them.
const (
	wantConflict      = "safehttp: WithoutProxy and RequireProxy are mutually exclusive"
	wantProxyRequired = "safehttp: RequireProxy set but no HTTP(S)_PROXY env var found — refusing to start (service.yaml has proxy_egress: true but /opt/_shared/proxy.env was not mounted)"
)

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(k, "")
	}
}

// panicValue runs f and returns what it panicked with (nil if it did not).
func panicValue(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

func TestNewClientE_ReturnsErrorForConflictingProxyOptions(t *testing.T) {
	c, err := safehttp.NewClientE(safehttp.WithoutProxy(), safehttp.RequireProxy())
	if err == nil {
		t.Fatal("NewClientE accepted WithoutProxy together with RequireProxy")
	}
	if err.Error() != wantConflict {
		t.Errorf("error = %q, want %q", err.Error(), wantConflict)
	}
	if c != nil {
		t.Error("a client was returned together with the error")
	}
}

func TestNewClientE_ReturnsErrorWhenRequiredProxyIsMissing(t *testing.T) {
	clearProxyEnv(t)
	for name, tc := range map[string]struct {
		env  string
		opts []safehttp.Option
	}{
		"RequireProxy option":      {opts: []safehttp.Option{safehttp.RequireProxy()}},
		"FLEET_REQUIRE_PROXY=1":    {env: "1"},
		"FLEET_REQUIRE_PROXY=true": {env: "true"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("FLEET_REQUIRE_PROXY", tc.env)
			c, err := safehttp.NewClientE(tc.opts...)
			if err == nil {
				t.Fatal("NewClientE succeeded without any proxy variable")
			}
			if err.Error() != wantProxyRequired {
				t.Errorf("error = %q, want %q", err.Error(), wantProxyRequired)
			}
			if c != nil {
				t.Error("a client was returned together with the error: a caller that ignores the error would reach the network without the proxy")
			}
		})
	}
}

func TestNewClientE_BuildsAClientWhenTheSetupIsValid(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("FLEET_REQUIRE_PROXY", "")

	// Plain defaults need no proxy.
	if c, err := safehttp.NewClientE(); err != nil || c == nil {
		t.Fatalf("NewClientE() = %v, %v", c, err)
	}
	// WithoutProxy wins over the fleet-wide switch, as for NewClient.
	t.Setenv("FLEET_REQUIRE_PROXY", "1")
	if c, err := safehttp.NewClientE(safehttp.WithoutProxy()); err != nil || c == nil {
		t.Fatalf("NewClientE(WithoutProxy) under FLEET_REQUIRE_PROXY=1 = %v, %v", c, err)
	}
	// A proxy variable satisfies RequireProxy.
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	if c, err := safehttp.NewClientE(safehttp.RequireProxy()); err != nil || c == nil {
		t.Fatalf("NewClientE(RequireProxy) with HTTPS_PROXY set = %v, %v", c, err)
	}
}

// NewClient keeps its behaviour: it still panics, with a string, and with the
// same text as before NewClientE existed (and as NewClientE's error).
func TestNewClient_StillPanicsWithTheSameText(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("FLEET_REQUIRE_PROXY", "")

	for name, tc := range map[string]struct {
		opts []safehttp.Option
		want string
	}{
		"conflicting options": {[]safehttp.Option{safehttp.WithoutProxy(), safehttp.RequireProxy()}, wantConflict},
		"proxy required":      {[]safehttp.Option{safehttp.RequireProxy()}, wantProxyRequired},
	} {
		t.Run(name, func(t *testing.T) {
			v := panicValue(func() { _ = safehttp.NewClient(tc.opts...) })
			s, ok := v.(string)
			if !ok {
				t.Fatalf("NewClient panicked with %T (%v), want the string it always used", v, v)
			}
			if s != tc.want {
				t.Errorf("panic = %q, want %q", s, tc.want)
			}
			_, err := safehttp.NewClientE(tc.opts...)
			if err == nil || fmt.Sprint(err) != s {
				t.Errorf("NewClientE error %v differs from the NewClient panic %q", err, s)
			}
		})
	}
}
