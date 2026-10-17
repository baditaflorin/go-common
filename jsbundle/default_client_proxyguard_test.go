package jsbundle

import (
	"context"
	"strings"
	"testing"
	"time"
)

// resetDefaultClient forgets the shared default client for one test and puts it
// back afterwards, so these tests do not depend on the order of the others.
func resetDefaultClient(t *testing.T) {
	t.Helper()
	defaultClientMu.Lock()
	prev := defaultClient
	defaultClient = nil
	defaultClientMu.Unlock()
	t.Cleanup(func() {
		defaultClientMu.Lock()
		defaultClient = prev
		defaultClientMu.Unlock()
	})
}

func misconfiguredProxyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FLEET_REQUIRE_PROXY", "1")
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(k, "")
	}
}

// The default client is built lazily, on the first Recover call, which is on a
// request path or inside a worker goroutine. safehttp.NewClient panics when
// FLEET_REQUIRE_PROXY is set and no proxy variable is present; that must come
// back as an error, and it must not leave the process with a nil default client.
func TestRecoverMisconfiguredProxyReturnsErrorNotPanic(t *testing.T) {
	misconfiguredProxyEnv(t)
	resetDefaultClient(t)

	for call := 1; call <= 2; call++ {
		var (
			err      error
			panicked any
		)
		func() {
			defer func() { panicked = recover() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = Recover(ctx, "https://example.com/app.js", RecoverOptions{})
		}()
		if panicked != nil {
			t.Fatalf("call %d: Recover panicked on a misconfigured proxy environment: %v", call, panicked)
		}
		if err == nil {
			t.Fatalf("call %d: Recover succeeded although FLEET_REQUIRE_PROXY=1 and no proxy variable is set", call)
		}
		if !strings.Contains(err.Error(), "RequireProxy") {
			t.Errorf("call %d: error does not say what is wrong with the proxy setup: %v", call, err)
		}
	}
}

func TestRecoverAllMisconfiguredProxyReturnsNothingNotPanic(t *testing.T) {
	misconfiguredProxyEnv(t)
	resetDefaultClient(t)

	var (
		got      []Source
		panicked any
	)
	func() {
		defer func() { panicked = recover() }()
		got = RecoverAll(context.Background(), []string{"https://example.com/app.js"}, RecoverOptions{})
	}()
	if panicked != nil {
		t.Fatalf("RecoverAll panicked on a misconfigured proxy environment: %v", panicked)
	}
	if len(got) != 0 {
		t.Fatalf("RecoverAll returned %d sources without a usable client", len(got))
	}
}

// A failed build must not poison the default client: once the environment is
// fixed, the next call builds it and works.
func TestDefaultClientRecoversAfterMisconfiguration(t *testing.T) {
	misconfiguredProxyEnv(t)
	resetDefaultClient(t)

	if c, err := guardedDefaultClient(); err == nil || c != nil {
		t.Fatalf("guardedDefaultClient() = %v, %v; want nil client and an error", c, err)
	}

	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	c, err := guardedDefaultClient()
	if err != nil || c == nil {
		t.Fatalf("guardedDefaultClient() after fixing the environment = %v, %v", c, err)
	}
	if again, _ := guardedDefaultClient(); again != c {
		t.Fatal("the client built after the failure should be cached and shared")
	}
}
