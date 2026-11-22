package egressproxy

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/proxysupplier"
)

func TestEgressProxyUsesPreselectedProviderNeutralRoute(t *testing.T) {
	upstream := newFakeUpstream(t, "route-user", "route-pass")
	proxyURLFile := filepath.Join(t.TempDir(), "route.proxy")
	if err := os.WriteFile(proxyURLFile, []byte(upstream.url()), 0600); err != nil {
		t.Fatal("write proxy URL file:", err)
	}
	router, err := proxysupplier.NewRouteRouter(proxysupplier.RouteRouterConfig{Providers: []proxysupplier.RouteProviderConfig{{
		ID: "thunder", Type: proxysupplier.RouteProviderStatic, Pools: []proxysupplier.StaticRoutePoolConfig{{
			ID: "trial", AccountID: "trial-account", Class: proxysupplier.PoolClassDC, ProxyURLFile: proxyURLFile,
		}},
	}}})
	if err != nil {
		t.Fatal("NewRouteRouter:", err)
	}
	t.Cleanup(router.Close)
	candidates, err := router.ResolveCandidates(proxysupplier.RouteIntent{PoolID: "trial"})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("resolve exact route: candidates=%#v err=%v", candidates, err)
	}
	selection, err := router.SelectRoute(candidates[0], false)
	if err != nil {
		t.Fatal("select route:", err)
	}
	target := newEchoServer(t)
	p := startProxy(t, func(c *Config) {
		c.Supplier = nil
		c.RouteSelection = &selection
	})
	_, resp, _ := connectVia(t, p.Addr(), target.addr())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	// The kernel completes the target's handshake before its accept goroutine
	// runs, so the counter can still read 0 right after the 200 reaches the
	// client (seen under go 1.27 on CI). Wait for it instead of reading it once.
	eventually(t, 3*time.Second, "the target to count the accepted connection", func() bool {
		return target.accepts.Load() == 1
	})
	if got := upstream.auths(); len(got) != 1 || got[0] != basicHeader("route-user", "route-pass") {
		t.Fatalf("upstream auth did not use selected route credentials (count=%d)", len(got))
	}
}

func TestRouteOutcomeObserverReceivesExactTransportFeedbackConcurrently(t *testing.T) {
	fakeUp := newFakeUpstream(t, "route-user", "route-pass")
	proxyURLFile := filepath.Join(t.TempDir(), "route.proxy")
	if err := os.WriteFile(proxyURLFile, []byte(fakeUp.url()), 0600); err != nil {
		t.Fatal("write proxy URL file:", err)
	}
	router, err := proxysupplier.NewRouteRouter(proxysupplier.RouteRouterConfig{Providers: []proxysupplier.RouteProviderConfig{{
		ID: "thunder", Type: proxysupplier.RouteProviderStatic, Pools: []proxysupplier.StaticRoutePoolConfig{{
			ID: "trial", AccountID: "trial-account", Class: proxysupplier.PoolClassDC, ProxyURLFile: proxyURLFile,
		}},
	}}})
	if err != nil {
		t.Fatal("NewRouteRouter:", err)
	}
	t.Cleanup(router.Close)
	candidates, err := router.ResolveCandidates(proxysupplier.RouteIntent{PoolID: "trial"})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("resolve exact route: candidates=%#v err=%v", candidates, err)
	}
	selection, err := router.SelectRoute(candidates[0], false)
	if err != nil {
		t.Fatal("select route:", err)
	}

	got := make(chan proxysupplier.RouteOutcome, 2)
	p := startProxy(t, func(c *Config) {
		c.Supplier = nil
		c.RouteSelection = &selection
		c.OnRouteOutcome = func(outcome proxysupplier.RouteOutcome) { got <- outcome }
	})
	var wg sync.WaitGroup
	for _, ok := range []bool{false, true} {
		ok := ok
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.markResult(&upstream{routeSelection: &selection}, ok)
		}()
	}
	wg.Wait()
	close(got)
	seen := map[proxysupplier.RouteOutcome]int{}
	for outcome := range got {
		seen[outcome]++
	}
	if seen[proxysupplier.RouteOutcomeSuccess] != 1 || seen[proxysupplier.RouteOutcomeTransportFailure] != 1 || len(seen) != 2 {
		t.Fatalf("observed route outcomes = %v", seen)
	}
}
