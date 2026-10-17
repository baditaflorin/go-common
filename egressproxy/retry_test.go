package egressproxy

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryOnSecondUpstreamAfterDialFailure(t *testing.T) {
	good := newFakeUpstream(t, fakeUser, fakePass)
	dead := "http://" + fakeUser + ":" + fakePass + "@" + closedAddr(t)
	deadAddr := dead[len("http://"+fakeUser+":"+fakePass+"@"):]
	echo := newEchoServer(t)
	sup := newSupplier(dead, good.url())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })

	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the retry on the second upstream to succeed", resp.StatusCode)
	}
	echoOnce(t, c, br, "retried")
	marks := sup.Marks()
	if len(marks) != 2 || marks[0] != (mark{deadAddr, false}) || marks[1] != (mark{good.addr(), true}) {
		t.Fatalf("marks = %v, want [(dead,false) (good,true)]", marks)
	}
	waitMetric(t, p.reg, "egressproxy_upstream_attempts_total", map[string]string{"result": "dial_error"}, 1)
	waitMetric(t, p.reg, "egressproxy_upstream_attempts_total", map[string]string{"result": "ok"}, 1)
}

func TestRetryOnSecondUpstreamAfter5xx(t *testing.T) {
	bad := newFakeUpstream(t, "", "")
	bad.setConnectStatus(http.StatusServiceUnavailable)
	good := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	sup := newSupplier(bad.url(), good.url())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })

	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	echoOnce(t, c, br, "ok")
	marks := sup.Marks()
	if len(marks) != 2 || marks[0] != (mark{bad.addr(), false}) || marks[1] != (mark{good.addr(), true}) {
		t.Fatalf("marks = %v", marks)
	}
	waitMetric(t, p.reg, "egressproxy_upstream_attempts_total", map[string]string{"result": "upstream_5xx"}, 1)
}

func TestAtMostTwoAttempts(t *testing.T) {
	a := newFakeUpstream(t, "", "")
	a.setConnectStatus(http.StatusBadGateway)
	b := newFakeUpstream(t, "", "")
	b.setConnectStatus(http.StatusBadGateway)
	third := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	sup := newSupplier(a.url(), b.url(), third.url())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })

	_, resp, _ := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want the upstream's 502 relayed", resp.StatusCode)
	}
	if third.requests() != 0 {
		t.Fatal("a third attempt was made")
	}
	if len(a.targets()) != 1 || len(b.targets()) != 1 {
		t.Fatalf("attempts: a=%d b=%d, want 1 each", len(a.targets()), len(b.targets()))
	}
	waitMetric(t, p.reg, "egressproxy_upstream_attempts_total", map[string]string{"result": "upstream_5xx"}, 2)
	waitMetric(t, p.reg, "egressproxy_requests_total", reqLabels("connect", "upstream_error"), 1)
}

func TestUpstream4xxIsRelayedWithoutRetryOrVerdict(t *testing.T) {
	a := newFakeUpstream(t, "", "")
	a.setConnectStatus(http.StatusForbidden)
	b := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	sup := newSupplier(a.url(), b.url())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })

	_, resp, _ := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want the upstream's 403 relayed", resp.StatusCode)
	}
	if b.requests() != 0 {
		t.Fatal("a 4xx was retried")
	}
	if m := sup.Marks(); len(m) != 0 {
		t.Fatalf("a 4xx produced a verdict on the exit IP: %v", m)
	}
}

func TestIgnoreUpstream5xx(t *testing.T) {
	a := newFakeUpstream(t, "", "")
	a.setConnectStatus(http.StatusBadGateway)
	b := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	sup := newSupplier(a.url(), b.url())
	p := startProxy(t, func(c *Config) {
		c.Supplier = sup
		c.IgnoreUpstream5xx = true
	})

	_, resp, _ := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 relayed", resp.StatusCode)
	}
	if b.requests() != 0 {
		t.Fatal("retried although IgnoreUpstream5xx is set")
	}
	if m := sup.Marks(); len(m) != 0 {
		t.Fatalf("marks = %v, want none", m)
	}
}

func TestHangingUpstreamIsBoundedByDialTimeout(t *testing.T) {
	echo := newEchoServer(t)
	hangA, hangB := hangListener(t), hangListener(t)
	sup := newSupplier("http://"+hangA, "http://"+hangB)
	p := startProxy(t, func(c *Config) {
		c.Supplier = sup
		c.DialTimeout = 600 * time.Millisecond
	})

	start := time.Now()
	_, resp, _ := connectVia(t, p.Addr(), echo.addr())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("took %v, want about DialTimeout (600ms) for both attempts together", elapsed)
	}
	if echo.accepts.Load() != 0 {
		t.Fatal("target dialed")
	}
	marks := sup.Marks()
	if len(marks) != 2 || marks[0].ok || marks[1].ok {
		t.Fatalf("marks = %v, want two failures", marks)
	}
}

func TestHangingFirstUpstreamStillLeavesTimeForTheRetry(t *testing.T) {
	good := newFakeUpstream(t, "", "")
	echo := newEchoServer(t)
	hang := hangListener(t)
	sup := newSupplier("http://"+hang, good.url())
	p := startProxy(t, func(c *Config) {
		c.Supplier = sup
		c.DialTimeout = time.Second
	})

	start := time.Now()
	c, resp, br := connectVia(t, p.Addr(), echo.addr())
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the retry to succeed", resp.StatusCode)
	}
	echoOnce(t, c, br, "late")
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("took %v", d)
	}
	marks := sup.Marks()
	if len(marks) != 2 || marks[0] != (mark{hang, false}) || marks[1] != (mark{good.addr(), true}) {
		t.Fatalf("marks = %v", marks)
	}
}

func TestPlainHTTPRetriesOnDialFailureAndMarks(t *testing.T) {
	good := newFakeUpstream(t, fakeUser, fakePass)
	dead := "http://" + fakeUser + ":" + fakePass + "@" + closedAddr(t)
	sup := newSupplier(dead, good.url())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })
	client := proxyClient(t, p.Proxy)

	resp, err := client.Get("http://origin.invalid/retry")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	marks := sup.Marks()
	if len(marks) != 2 || marks[0].ok || !marks[1].ok {
		t.Fatalf("marks = %v", marks)
	}
}

func TestPlainHTTPWrongCredentialsIs502(t *testing.T) {
	up := newFakeUpstream(t, fakeUser, fakePass)
	sup := newSupplier("http://" + fakeUser + ":wrong@" + up.addr())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })
	client := proxyClient(t, p.Proxy)

	resp, err := client.Get("http://origin.invalid/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Proxy-Authenticate") != "" {
		t.Fatalf("status = %d, headers %v", resp.StatusCode, resp.Header)
	}
	for _, m := range sup.Marks() {
		if m.ok {
			t.Fatalf("marks = %v", sup.Marks())
		}
	}
	if len(sup.Marks()) == 0 {
		t.Fatal("no failure was reported")
	}
}

func TestPlainHTTPPostIsNotRetriedAfterTheBodyWasSent(t *testing.T) {
	// A POST whose exchange failed after the body went out must not be sent
	// again: the first upstream resets, the second must stay untouched.
	reset := hangListenerThatCloses(t)
	good := newFakeUpstream(t, "", "")
	sup := newSupplier("http://"+reset, good.url())
	p := startProxy(t, func(c *Config) { c.Supplier = sup })
	client := proxyClient(t, p.Proxy)

	resp, err := client.Post("http://origin.invalid/pay", "text/plain", stringsReader("amount=1"))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
	}
	if good.requests() != 0 {
		t.Fatal("a POST was replayed on a second upstream after its body had been sent")
	}
}
