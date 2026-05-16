package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/client"
	"github.com/baditaflorin/go-common/config"
)

func TestSchema_DefaultsToOne(t *testing.T) {
	cfg := &config.Config{AppName: "go_default_schema", Version: "1.0.0", Port: "0"}
	srv := New(cfg)

	req := httptest.NewRequest("GET", "/schema", nil)
	rr := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var got struct {
		Service       string `json:"service"`
		Version       string `json:"version"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Service != "go_default_schema" {
		t.Errorf("service: got %q", got.Service)
	}
	if got.Version != "1.0.0" {
		t.Errorf("version: got %q", got.Version)
	}
	if got.SchemaVersion != 1 {
		t.Errorf("default schema_version: got %d want 1", got.SchemaVersion)
	}
}

func TestSchema_WithSchemaVersionThree(t *testing.T) {
	cfg := &config.Config{AppName: "go_bumped", Version: "2.4.1", Port: "0"}
	srv := New(cfg, WithSchemaVersion(3))

	req := httptest.NewRequest("GET", "/schema", nil)
	rr := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := got["schema_version"].(float64); !ok || int(v) != 3 {
		t.Errorf("schema_version: got %v want 3", got["schema_version"])
	}

	if CurrentSchemaVersion() != 3 {
		t.Errorf("CurrentSchemaVersion(): got %d want 3", CurrentSchemaVersion())
	}
}

func TestSchema_NoAuthRequired(t *testing.T) {
	// /schema is metadata, not data — it must respond without any
	// Authorization or X-API-Key header. Mounted on the mux directly,
	// not behind middleware that would 401 the request.
	cfg := &config.Config{AppName: "go_no_auth", Version: "0.1.0", Port: "0"}
	srv := New(cfg, WithSchemaVersion(2))

	req := httptest.NewRequest("GET", "/schema", nil)
	// Explicitly no auth headers.
	rr := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 without auth, got %d (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestSchema_CapabilitiesEchoesSchemaVersion(t *testing.T) {
	cfg := &config.Config{AppName: "go_cap_schema", Version: "0.0.1", Port: "0"}
	srv := New(cfg,
		WithSchemaVersion(7),
		WithCapability(client.Capability{Name: "x", Type: "bool"}),
	)

	req := httptest.NewRequest("GET", "/capabilities", nil)
	rr := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rr, req)

	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := got["schema_version"].(float64); !ok || int(v) != 7 {
		t.Errorf("capabilities.schema_version: got %v want 7", got["schema_version"])
	}
}
