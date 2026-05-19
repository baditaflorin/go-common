package fleetfetch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/fleetfetch"
)

func TestLoopbackClient_Get(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello loopback"))
	}))
	defer srv.Close()

	var fetchClient fleetfetch.Doer = fleetfetch.NewLoopbackClient(srv.Client())
	r, err := fetchClient.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.Status != 200 {
		t.Errorf("Status = %d", r.Status)
	}
	if string(r.Body) != "hello loopback" {
		t.Errorf("body = %q", r.Body)
	}
	if r.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
	}
}

func TestLoopbackClient_GetWithHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the Accept header so the test can verify it was forwarded.
		_, _ = w.Write([]byte(r.Header.Get("Accept")))
	}))
	defer srv.Close()
	fetchClient := fleetfetch.NewLoopbackClient(srv.Client())
	r, err := fetchClient.GetWithHeaders(context.Background(), srv.URL, http.Header{
		"Accept": []string{"application/rdap+json"},
	})
	if err != nil {
		t.Fatalf("GetWithHeaders: %v", err)
	}
	if !strings.Contains(string(r.Body), "application/rdap+json") {
		t.Errorf("Accept not forwarded; body = %q", r.Body)
	}
}

func TestLoopbackClient_NilHTTPDefaults(t *testing.T) {
	c := fleetfetch.NewLoopbackClient(nil)
	if c.HTTP != http.DefaultClient {
		t.Errorf("nil should default to http.DefaultClient")
	}
}

func TestDoerInterfaceSatisfaction(t *testing.T) {
	// Compile-time check is already in doer.go via var _ Doer = ...; this
	// adds a runtime smoke test in case the file is split during refactor.
	var _ fleetfetch.Doer = fleetfetch.NewClient()
	var _ fleetfetch.Doer = fleetfetch.NewLoopbackClient(nil)
}
