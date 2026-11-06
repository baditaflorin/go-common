package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/cache"
	"github.com/baditaflorin/go-common/clock"
)

func TestProxyDecisionCacheIsBoundedAndExpires(t *testing.T) {
	mc := clock.NewMock(time.Unix(0, 0))
	decisions := cache.New[string, bool](3, time.Minute, cache.WithClock[string, bool](mc))
	calls := 0
	addresses := []string{"93.184.216.34"}
	lookup := func(context.Context, string) ([]string, error) { calls++; return addresses, nil }
	request := func(host string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		return r
	}
	for i := range 100 {
		got, err := proxySkippingPrivateWithLookup(request(fmt.Sprintf("host%d.example.com", i)), viaFakeProxy, lookup, decisions)
		if err != nil || got != fakeProxy {
			t.Fatalf("public host proxy=%v err=%v", got, err)
		}
	}
	if got := decisions.Len(); got != 3 {
		t.Fatalf("cache size=%d, want 3", got)
	}
	req := request("host99.example.com")
	addresses = []string{"10.0.0.1"}
	got, _ := proxySkippingPrivateWithLookup(req, viaFakeProxy, lookup, decisions)
	if got != fakeProxy || calls != 100 {
		t.Fatalf("fresh decision was not reused: proxy=%v lookups=%d", got, calls)
	}
	mc.Advance(time.Minute)
	got, err := proxySkippingPrivateWithLookup(req, viaFakeProxy, lookup, decisions)
	if err != nil || got != nil || calls != 101 {
		t.Fatalf("expired decision was not resolved again: proxy=%v lookups=%d err=%v", got, calls, err)
	}
	// A direct-routing decision never authorizes a private target.
	if !IsBlockedForHost("host99.example.com", net.ParseIP(addresses[0])) {
		t.Fatal("proxy decision bypassed SSRF policy")
	}
}

func TestProxyDecisionCacheDoesNotRememberDNSErrors(t *testing.T) {
	decisions := cache.New[string, bool](3, time.Minute)
	calls := 0
	lookup := func(context.Context, string) ([]string, error) { calls++; return nil, errors.New("DNS unavailable") }
	req, _ := http.NewRequest(http.MethodGet, "https://error.example.com/", nil)
	for range 2 {
		got, err := proxySkippingPrivateWithLookup(req, viaFakeProxy, lookup, decisions)
		if err != nil || got != fakeProxy {
			t.Fatalf("DNS failure changed proxy route: %v %v", got, err)
		}
	}
	if calls != 2 || decisions.Len() != 0 {
		t.Fatal("transient DNS failure was cached")
	}
}

func BenchmarkProxyDecisionCacheHit(b *testing.B) {
	decisions := cache.New[string, bool](16, time.Minute)
	decisions.Set("example.com", false)
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	lookup := func(context.Context, string) ([]string, error) { b.Fatal("cache miss"); return nil, nil }
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = proxySkippingPrivateWithLookup(req, viaFakeProxy, lookup, decisions)
	}
}
