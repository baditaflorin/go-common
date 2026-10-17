package egressproxy

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestNoUpstreamFailsClosedWithoutDialingTheTarget(t *testing.T) {
	never := newEchoServer(t) // must never be contacted
	sup := newSupplier()      // returns ""
	p := startProxy(t, func(c *Config) { c.Supplier = sup })

	_, resp, _ := connectVia(t, p.Addr(), never.addr())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("CONNECT status = %d, want 503", resp.StatusCode)
	}
	client := proxyClient(t, p.Proxy)
	r, err := client.Get("http://" + never.addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("plain status = %d, want 503", r.StatusCode)
	}

	time.Sleep(200 * time.Millisecond) // a stray direct dial would have landed by now
	if n := never.accepts.Load(); n != 0 {
		t.Fatalf("the target was dialed %d times with no upstream available", n)
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "no_upstream"), 1)
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("http", "no_upstream"), 1)
}

func TestSelfProxyLoopIsRefused(t *testing.T) {
	never := newEchoServer(t)
	var self atomic.Pointer[Proxy]
	sup := funcSupplier(func() string {
		if p := self.Load(); p != nil {
			return "http://u:p@" + p.Addr() // the supplier points back at this proxy
		}
		return ""
	})
	p := startProxy(t, func(c *Config) { c.Supplier = sup })
	self.Store(p.Proxy)

	_, resp, _ := connectVia(t, p.Addr(), never.addr())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a self-proxy loop", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if never.accepts.Load() != 0 {
		t.Fatal("target dialed")
	}
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "no_upstream"), 1)
}

func TestNilSupplierWithoutRequireUpstreamStillRefuses(t *testing.T) {
	never := newEchoServer(t)
	p := startProxy(t, func(c *Config) { c.RequireUpstream = false })
	_, resp, _ := connectVia(t, p.Addr(), never.addr())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if never.accepts.Load() != 0 {
		t.Fatal("RequireUpstream=false opened a direct path")
	}
}

func TestUnusableUpstreamURLsAreRefusedAndNotDialed(t *testing.T) {
	never := newEchoServer(t)
	for name, raw := range map[string]string{
		"socks scheme": "socks5://u:p@127.0.0.1:1080",
		"bad host":     "http://u:p@bad host:80",
		"no host":      "http://u:p@",
		"garbage":      "::::",
	} {
		t.Run(name, func(t *testing.T) {
			p := startProxy(t, func(c *Config) { c.Supplier = newSupplier(raw) })
			_, resp, _ := connectVia(t, p.Addr(), never.addr())
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", resp.StatusCode)
			}
			waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "upstream_error"), 1)
		})
	}
	if never.accepts.Load() != 0 {
		t.Fatal("target dialed")
	}
}
