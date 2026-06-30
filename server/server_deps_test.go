package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/depcheck"
	"github.com/baditaflorin/go-common/server"
)

func TestHealthWithoutDeps(t *testing.T) {
	cfg := &config.Config{AppName: "test-svc", Version: "0.0.1", Port: "0"}
	srv := server.New(cfg)
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	var m map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["status"] != "ok" {
		t.Errorf("status: %v", m["status"])
	}
	if _, hasDeps := m["dependencies"]; hasDeps {
		t.Errorf("no deps registered, key should be absent")
	}
}

func TestHealthWithAllDepsOK(t *testing.T) {
	reg := depcheck.New()
	reg.Register("ok-dep", func(ctx context.Context) error { return nil })

	cfg := &config.Config{AppName: "test-svc", Version: "0.0.1", Port: "0"}
	srv := server.New(cfg, server.WithDependencies(reg))

	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	var m map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&m)
	if m["status"] != "ok" {
		t.Errorf("status: %v", m["status"])
	}
	deps, ok := m["dependencies"].([]interface{})
	if !ok || len(deps) != 1 {
		t.Fatalf("dependencies: %v", m["dependencies"])
	}
}

func TestHealthWithFailingDepDegrades(t *testing.T) {
	reg := depcheck.New()
	reg.Register("bad", func(ctx context.Context) error { return errors.New("boom") })

	cfg := &config.Config{AppName: "test-svc", Version: "0.0.1", Port: "0"}
	srv := server.New(cfg, server.WithDependencies(reg))

	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status code should stay 200, got %d", rec.Code)
	}
	var m map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&m)
	if m["status"] != "degraded" {
		t.Errorf("expected degraded, got %v", m["status"])
	}
}
