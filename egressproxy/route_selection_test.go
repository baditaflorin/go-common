package egressproxy

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

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
	if target.accepts.Load() != 1 {
		t.Fatalf("target accepts = %d, want 1", target.accepts.Load())
	}
	if got := upstream.auths(); len(got) != 1 || got[0] != basicHeader("route-user", "route-pass") {
		t.Fatalf("upstream auth did not use selected route credentials (count=%d)", len(got))
	}
}
