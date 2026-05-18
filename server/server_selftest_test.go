package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/server"
)

// TestDefaultSelftestServes200 verifies that a service which did not
// register its own /selftest still answers 200 to the fleet smoke
// gate. Pre-fix, fleet-runner deploy auto-rolled-back any service
// whose catchall handler returned 4xx/5xx for /selftest (e.g.
// go_url_shortener returning 400 "Missing 'target' parameter").
func TestDefaultSelftestServes200(t *testing.T) {
	cfg := &config.Config{AppName: "test-svc", Version: "9.9.9", Port: "0"}
	srv := server.New(cfg)

	// Use the test http.Server with the same final-handler wrap as
	// production to exercise wrapDefaults.
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/selftest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/selftest: want 200, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["service"] != "test-svc" {
		t.Errorf("service: got %v, want test-svc", body["service"])
	}
	if body["status"] != "ok" {
		t.Errorf("status: got %v, want ok", body["status"])
	}
}

// TestUserSelftestWinsOverDefault verifies that a service that DOES
// register /selftest gets its handler called, not the default.
func TestUserSelftestWinsOverDefault(t *testing.T) {
	cfg := &config.Config{AppName: "test-svc", Version: "9.9.9", Port: "0"}
	srv := server.New(cfg)
	srv.Mux.HandleFunc("/selftest", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"mine":true}`))
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/selftest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("user handler should win: want 418, got %d", resp.StatusCode)
	}
}

// TestDefaultSelftestDoesNotShadowCatchall verifies that the
// default wrap does not steal requests for unrelated paths.
func TestDefaultSelftestDoesNotShadowCatchall(t *testing.T) {
	cfg := &config.Config{AppName: "test-svc", Version: "9.9.9", Port: "0"}
	srv := server.New(cfg)
	srv.Mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/some-other-path")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("catchall should still handle /some-other-path: got %d", resp.StatusCode)
	}
}
