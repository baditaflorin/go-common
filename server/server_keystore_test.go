package server

import (
	"testing"

	"github.com/baditaflorin/go-common/config"
)

// TestWithKeystoreAuth_Wires confirms the option mounts a middleware
// without blowing up at construction time, even with no env vars set
// (apikey.New uses defaults; failures are deferred to first request).
func TestWithKeystoreAuth_Wires(t *testing.T) {
	cfg := &config.Config{AppName: "test", Version: "0.0.0", Port: "0"}
	srv := New(cfg, WithKeystoreAuth("default_token"))
	if srv == nil {
		t.Fatal("server is nil")
	}
	// at least the three defaults plus one we just added
	if len(srv.Middlewares) < 4 {
		t.Fatalf("expected ≥4 middlewares (3 default + keystore auth), got %d",
			len(srv.Middlewares))
	}
}

// TestWithKeystoreAuthTier_Wires mirrors TestWithKeystoreAuth_Wires for the
// tier-gated variant — construction must not blow up regardless of
// requiredTier/enforce, since actual deny/allow behavior is covered by
// middleware's own TokenAuthKeystore tests.
func TestWithKeystoreAuthTier_Wires(t *testing.T) {
	cfg := &config.Config{AppName: "test", Version: "0.0.0", Port: "0"}
	srv := New(cfg, WithKeystoreAuthTier("vetted-pentest", true, "default_token"))
	if srv == nil {
		t.Fatal("server is nil")
	}
	if len(srv.Middlewares) < 4 {
		t.Fatalf("expected ≥4 middlewares (3 default + keystore auth), got %d",
			len(srv.Middlewares))
	}
}
