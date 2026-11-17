package egressproxy

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestAllowTargetRunsAfterGuardAndBeforeUpstreamDial(t *testing.T) {
	upstream := newEchoServer(t)
	var guardCalls, policyCalls atomic.Int64
	p := startProxy(t, func(c *Config) {
		c.Supplier = newSupplier("http://" + upstream.addr())
		c.Guard = func(context.Context, string, string) error {
			guardCalls.Add(1)
			return nil
		}
		c.AllowTarget = func(host string) bool {
			if guardCalls.Load() != 1 {
				t.Errorf("destination policy ran before canonical guard: guard calls=%d", guardCalls.Load())
			}
			if host != "example.org" {
				t.Errorf("AllowTarget host = %q, want normalized example.org", host)
			}
			policyCalls.Add(1)
			return false
		}
	})
	_, resp, _ := connectVia(t, p.Addr(), "EXAMPLE.org.:443")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT status = %d, want 403", resp.StatusCode)
	}
	if guardCalls.Load() != 1 || policyCalls.Load() != 1 || upstream.accepts.Load() != 0 {
		t.Fatalf("guard=%d policy=%d upstream_accepts=%d", guardCalls.Load(), policyCalls.Load(), upstream.accepts.Load())
	}
}
