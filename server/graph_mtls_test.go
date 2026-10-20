package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/spiffe"
)

// graphMTLSClient must return a nil interface with the error, never a typed
// nil *spiffe.HTTPClient that graph would take for a connected client.
func TestGraphMTLSClientFailsWithANilClient(t *testing.T) {
	for _, socket := range []string{"", "tcp://127.0.0.1:8081", "unix:///nonexistent/agent.sock"} {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		client, err := graphMTLSClient(ctx, socket, []string{"spiffe://0exec.com/ns/fleet/sa/fleet-graph"}, 5*time.Second)
		cancel()
		if err == nil || client != nil {
			t.Fatalf("graphMTLSClient(%q) = %v, %v; want nil and an error", socket, client, err)
		}
		if !errors.Is(err, spiffe.ErrUnavailable) {
			t.Fatalf("graphMTLSClient(%q) error = %v; want spiffe.ErrUnavailable", socket, err)
		}
	}
}
