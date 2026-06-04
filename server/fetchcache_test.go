package server_test

import (
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/safehttp"
	"github.com/baditaflorin/go-common/server"
)

// When FLEET_FETCH_CACHE_URL is set, server.New installs a process-wide
// safehttp fetch delegate so every subsequently-constructed safehttp
// client transparently routes its GETs through the fleet fetch cache.
func TestNew_WiresFetchCacheDelegate(t *testing.T) {
	// Ensure a clean baseline regardless of test ordering.
	safehttp.SetDefaultFetchDelegate(nil)
	t.Cleanup(func() { safehttp.SetDefaultFetchDelegate(nil) })

	t.Setenv(fleetfetch.EnvCacheURL, "http://go_infrastructure_fetch_cache:18205")

	cfg := &config.Config{AppName: "go_fetchcache_wiring_test", Version: "0.0.0", Port: "0"}
	_ = server.New(cfg)

	if safehttp.DefaultFetchDelegate() == nil {
		t.Fatalf("expected a default fetch delegate to be installed when %s is set", fleetfetch.EnvCacheURL)
	}
}

// Without the env var, server.New must NOT install a delegate (no
// silent fleet-wide egress rerouting on services that didn't opt in).
func TestNew_NoFetchCacheDelegateWithoutEnv(t *testing.T) {
	safehttp.SetDefaultFetchDelegate(nil)
	t.Cleanup(func() { safehttp.SetDefaultFetchDelegate(nil) })

	// t.Setenv with empty value guarantees the var is unset for this test
	// and restored afterward.
	t.Setenv(fleetfetch.EnvCacheURL, "")

	cfg := &config.Config{AppName: "go_fetchcache_noenv_test", Version: "0.0.0", Port: "0"}
	_ = server.New(cfg)

	if safehttp.DefaultFetchDelegate() != nil {
		t.Fatalf("did not expect a fetch delegate when %s is unset", fleetfetch.EnvCacheURL)
	}
}
