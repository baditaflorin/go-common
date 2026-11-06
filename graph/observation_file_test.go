package graph

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestObservationDoesNotReadRotatingCredentialFile(t *testing.T) {
	resetState(t)
	setGraphEnv(t, unreachableCollectorURL)
	path := filepath.Join(t.TempDir(), "writer-key")
	if err := os.WriteFile(path, []byte("test-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRAPH_API_KEY_FILE", path)
	Init("service", "test")
	t.Cleanup(func() { _ = Shutdown() })
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !observing() {
		t.Fatal("observation consulted a missing credential file")
	}
	Record(Event{Direction: "out", Target: "target", Path: "/work", Method: "GET"})
	posts := 0
	s := ensureInit().sender
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		posts++
		if r.Header.Get("X-API-Key") != "rotated-key" {
			t.Error("sender did not reload the rotated credential")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	s.flush()
	if posts != 0 || ensureInit().ring.len() != 1 {
		t.Fatal("sender failed open or lost buffered event while key missing")
	}
	if err := os.WriteFile(path, []byte("rotated-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.flush()
	if posts != 1 || ensureInit().ring.len() != 0 {
		t.Fatal("sender did not deliver after credential rotation")
	}
}

func BenchmarkGraphObservingFileCredential(b *testing.B) {
	b.Setenv("GRAPH_ENABLED", "true")
	b.Setenv("GRAPH_COLLECTOR_URL", "http://127.0.0.1:1")
	b.Setenv("GRAPH_SERVICE_ID", "")
	path := filepath.Join(b.TempDir(), "writer-key")
	if err := os.WriteFile(path, []byte("benchmark-only"), 0o600); err != nil {
		b.Fatal(err)
	}
	b.Setenv("GRAPH_API_KEY_FILE", path)
	Init("service", "test")
	b.Cleanup(func() { _ = Shutdown() })
	b.ReportAllocs()
	for b.Loop() {
		if !observing() {
			b.Fatal("observation disabled")
		}
	}
}
