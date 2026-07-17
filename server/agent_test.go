package server

import (
	"embed"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/agent"
	"github.com/baditaflorin/go-common/config"
)

//go:embed testdata/agent.json
var agentTestFS embed.FS

func TestWithAgentFromEmbedServesPublicContract(t *testing.T) {
	cfg := config.Load("test-agent-svc", "1.0.0")
	srv := New(cfg, WithAgentFromEmbed(agentTestFS, "testdata/agent.json"))

	// /agent.json must be reachable WITHOUT auth (an agent reads the
	// contract before it has a key).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent.json", nil)
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/agent.json status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body := rec.Body.String()
	if body == "" || body[0] != '{' {
		t.Fatalf("/agent.json body not JSON: %q", body)
	}
}

func TestWithAgentServesDefaultContract(t *testing.T) {
	cfg := config.Load("test-agent-default", "2.0.0")
	srv := New(cfg, WithAgent(agent.DefaultContract(cfg.AppName, cfg.Version)))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent.json", nil)
	srv.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/agent.json status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), `"service": "test-agent-default"`) && !contains(rec.Body.String(), `"service":"test-agent-default"`) {
		t.Fatalf("/agent.json missing service identity: %s", rec.Body.String())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
