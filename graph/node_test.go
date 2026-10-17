package graph

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// nodeCollector records the X-Fleet-Node header and body of every batch it receives.
type nodeCollector struct {
	mu      sync.Mutex
	headers []string
	bodies  []string
	present []bool
}

func (c *nodeCollector) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.headers = append(c.headers, r.Header.Get(NodeHeader))
		_, has := r.Header[http.CanonicalHeaderKey(NodeHeader)]
		c.present = append(c.present, has)
		c.bodies = append(c.bodies, string(body))
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
}

// sendOneBatch wires a writer to a fresh collector with FLEET_NODE=node, records one event and flushes it.
func sendOneBatch(t *testing.T, node string) *nodeCollector {
	t.Helper()
	resetState(t)
	nodeInvalidLogOnce = sync.Once{}
	c := &nodeCollector{}
	srv := httptest.NewServer(c.handler())
	t.Cleanup(srv.Close)
	setGraphEnv(t, srv.URL)
	t.Setenv("FLEET_NODE", node)
	Init("go_example_service", "1.0.0")
	t.Cleanup(func() { _ = Shutdown() })
	Record(Event{Direction: "out", Target: "example.com", Method: http.MethodGet, Status: http.StatusOK})
	ensureInit().sender.flush()
	return c
}

func TestBatchCarriesTheNodeHeaderAndNoJSONField(t *testing.T) {
	c := sendOneBatch(t, "Infer-Runtime")
	if len(c.headers) != 1 || c.headers[0] != "infer-runtime" {
		t.Fatalf("X-Fleet-Node = %v, want one batch carrying the lowercased node infer-runtime", c.headers)
	}
	if got := Node(); got != "infer-runtime" {
		t.Fatalf("Node() = %q, want infer-runtime", got)
	}
	// An older collector decodes batches with DisallowUnknownFields: the node must NOT be a JSON field.
	if strings.Contains(c.bodies[0], `"node"`) {
		t.Fatalf("the node leaked into the JSON body, which an older collector rejects: %s", c.bodies[0])
	}
}

func TestNoNodeHeaderWhenFleetNodeIsUnset(t *testing.T) {
	c := sendOneBatch(t, "")
	if len(c.present) != 1 || c.present[0] {
		t.Fatalf("batch carried an X-Fleet-Node header (%v) with FLEET_NODE unset", c.headers)
	}
	if Node() != "" {
		t.Fatalf("Node() = %q, want empty", Node())
	}
}

func TestInvalidFleetNodeIsIgnoredAndLoggedOnce(t *testing.T) {
	for _, bad := range []string{"infer_runtime", "infer runtime", "a\r\nX-Evil: 1", "-x", strings.Repeat("a", 64)} {
		buf := captureLog(t)
		c := sendOneBatch(t, bad)
		if len(c.present) != 1 || c.present[0] {
			t.Errorf("FLEET_NODE=%q produced header %v, want none", bad, c.headers)
		}
		if got := len(logLines(buf)); got != 1 {
			t.Errorf("FLEET_NODE=%q logged %d lines, want exactly 1: %s", bad, got, buf.String())
		}
		if line := buf.String(); !strings.Contains(line, "FLEET_NODE") || strings.Contains(line, "Evil") {
			t.Errorf("log line should name FLEET_NODE and not echo the value: %q", line)
		}
	}
}
