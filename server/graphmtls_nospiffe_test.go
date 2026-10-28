//go:build gocommon_nospiffe

package server

import (
	"context"
	"testing"
	"time"
)

// In a binary built with -tags gocommon_nospiffe the SPIFFE path says what is missing:
// a nil client (never a typed nil) and an error that names the tag.
func TestGraphMTLSClientNamesTheTagItWasBuiltWith(t *testing.T) {
	client, err := graphMTLSClient(context.Background(), "unix:///nonexistent/agent.sock", []string{"spiffe://0exec.com/ns/fleet/sa/fleet-graph"}, time.Second)
	if client != nil || err != errNoSPIFFE {
		t.Fatalf("graphMTLSClient = %v, %v; want nil and errNoSPIFFE", client, err)
	}
}
