package egressproxy

import (
	"net/http"
	"testing"

	"github.com/baditaflorin/go-common/testhelpers/leakcheck"
)

// A tunnel that was opened, used and closed, and a proxy that was then closed,
// must leave no goroutine behind: the splice, accept loop and per-connection
// goroutines all have to finish.
func TestTunnelAndCloseLeaveNoGoroutines(t *testing.T) {
	leakcheck.Check(t) // registered first, so it runs after every cleanup below

	up := newFakeUpstream(t, fakeUser, fakePass)
	echo := newEchoServer(t)
	p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(up.url()) })

	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	echoOnce(t, c, br, "through the tunnel")
	c.Close()
	waitMetric(t, p.reg, "egressproxy_active_tunnels", nil, 0)
}
