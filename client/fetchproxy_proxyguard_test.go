package client

import (
	"context"
	"strings"
	"testing"
)

// safehttp.NewClient panics when FLEET_REQUIRE_PROXY is set and no proxy
// variable is present, which is right at start-up. Fetch's direct path builds
// its safehttp client on every call, so that panic used to escape from the
// request handler that called Fetch, or kill the process when a worker
// goroutine did. A deployment problem must surface as an error the caller can
// return, and it must stay fail-closed: nothing is fetched.
func TestFetchDirectMisconfiguredProxyReturnsErrorNotPanic(t *testing.T) {
	t.Setenv("FLEET_REQUIRE_PROXY", "1")
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "HTML_PROXY_URL"} {
		t.Setenv(k, "")
	}

	var (
		res      *FetchResult
		err      error
		panicked any
	)
	func() {
		defer func() { panicked = recover() }()
		res, err = Fetch(context.Background(), "https://example.com/", WithAllowDirect(true))
	}()

	if panicked != nil {
		t.Fatalf("Fetch panicked on a misconfigured proxy environment instead of returning an error: %v", panicked)
	}
	if err == nil {
		t.Fatal("Fetch succeeded although FLEET_REQUIRE_PROXY=1 and no proxy variable is set")
	}
	if res != nil {
		t.Errorf("Fetch returned a result together with the error: %+v", res)
	}
	if !strings.Contains(err.Error(), "RequireProxy") {
		t.Errorf("error does not say what is wrong with the proxy setup: %v", err)
	}
}
