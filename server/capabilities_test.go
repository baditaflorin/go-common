package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/client"
	"github.com/baditaflorin/go-common/config"
)

func TestCapabilities_EndpointReturnsRegisteredFlags(t *testing.T) {
	cfg := &config.Config{AppName: "go_demo", Version: "9.9.9", Port: "0"}

	srv := New(cfg,
		WithCapability(client.FetchCapabilities...),
		WithCapability(client.Capability{
			Name:        "vendor",
			Description: "Restrict to one vendor",
			Type:        "string",
		}),
	)

	req := httptest.NewRequest("GET", "/capabilities", nil)
	rr := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var got struct {
		Service      string              `json:"service"`
		Version      string              `json:"version"`
		Capabilities []client.Capability `json:"capabilities"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Service != "go_demo" || got.Version != "9.9.9" {
		t.Errorf("identity wrong: %+v", got)
	}
	if len(got.Capabilities) < 3 {
		t.Errorf("expected at least 3 capabilities (use_js, use_network, vendor), got %d", len(got.Capabilities))
	}
	names := map[string]bool{}
	for _, c := range got.Capabilities {
		names[c.Name] = true
	}
	for _, want := range []string{"use_js", "use_network", "vendor"} {
		if !names[want] {
			t.Errorf("capabilities missing %q", want)
		}
	}
}

func TestCapabilities_EmptyWhenUnregistered(t *testing.T) {
	cfg := &config.Config{AppName: "go_bare", Version: "0.0.1", Port: "0"}
	srv := New(cfg)

	req := httptest.NewRequest("GET", "/capabilities", nil)
	rr := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var got struct {
		Capabilities []client.Capability `json:"capabilities"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Capabilities) != 0 {
		t.Errorf("expected empty capabilities when none registered, got %+v", got.Capabilities)
	}
}
