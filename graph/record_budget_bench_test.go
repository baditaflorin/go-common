package graph

import "testing"

func BenchmarkRecordBoundedStrings(b *testing.B) {
	// Avoid real network activity; the timer never needs to fire during a
	// benchmark sample, and shutdown goes only to an unreachable loopback.
	b.Setenv("GRAPH_ENABLED", "true")
	b.Setenv("GRAPH_COLLECTOR_URL", "http://127.0.0.1:1")
	b.Setenv("GRAPH_API_KEY", "benchmark-only")
	b.Setenv("GRAPH_SERVICE_ID", "")
	b.Setenv("GRAPH_FLUSH_INTERVAL", "3600")
	b.Setenv("GRAPH_BUFFER_SIZE", "64")
	Init("service", "test")
	b.Cleanup(func() { _ = Shutdown() })
	e := Event{Direction: "out", Target: "external:example.com", Path: "/api/{id}", Method: "GET", Status: 200}
	b.ReportAllocs()
	for b.Loop() {
		Record(e)
	}
}
