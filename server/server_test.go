package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/server"
)

func TestHealthHandler(t *testing.T) {
	h := server.HealthHandler("go_test_svc", "1.2.3")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var m map[string]string
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["status"] != "ok" {
		t.Errorf("status: got %q, want ok", m["status"])
	}
	if m["service"] != "go_test_svc" {
		t.Errorf("service: got %q, want go_test_svc", m["service"])
	}
	if m["version"] != "1.2.3" {
		t.Errorf("version: got %q, want 1.2.3", m["version"])
	}
}

func TestVersionHandler(t *testing.T) {
	h := server.VersionHandler("2.0.0")
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var m map[string]string
	json.NewDecoder(w.Body).Decode(&m)
	if m["version"] != "2.0.0" {
		t.Errorf("version: got %q, want 2.0.0", m["version"])
	}
}
