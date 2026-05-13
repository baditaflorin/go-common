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
