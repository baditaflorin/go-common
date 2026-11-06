package proxysupplier

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type changingSupplier struct{ address string }

func (s *changingSupplier) Name() string     { return "test" }
func (s *changingSupplier) ProxyURL() string { return s.address }

type bypassingEmptySupplier struct{ changingSupplier }

func (*bypassingEmptySupplier) Bypass(host string) bool { return host == "internal.example" }

func TestFailClosedClientNeverGoesDirectWhilePoolEmpty(t *testing.T) {
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	s := &changingSupplier{}
	client := HTTPClientFailClosed(s, time.Second)
	_, err := client.Get(target.URL)
	if !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("empty pool: got %v, want ErrProxyUnavailable", err)
	}
	if hits.Load() != 0 {
		t.Fatal("empty pool sent a direct request")
	}
	// Once the supplier recovers, the same client begins using it.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	s.address = proxy.URL
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("recovered proxy: %v", err)
	}
	resp.Body.Close()
	if hits.Load() != 0 {
		t.Fatal("recovered request did not use proxy")
	}
}

func TestLegacyClientStillAllowsEmptySupplier(t *testing.T) {
	s := &changingSupplier{}
	client := HTTPClient(s, time.Second)
	tr := client.Transport.(*graphObservedTransport)
	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	if proxy, err := tr.Proxy(req); proxy != nil || err != nil {
		t.Fatalf("legacy empty supplier = %v, %v", proxy, err)
	}
}

func TestFailClosedClientHonorsExplicitBypass(t *testing.T) {
	client := HTTPClientFailClosed(&bypassingEmptySupplier{}, time.Second)
	tr := client.Transport.(*graphObservedTransport)
	req := httptest.NewRequest(http.MethodGet, "http://internal.example/", nil)
	if proxy, err := tr.Proxy(req); proxy != nil || err != nil {
		t.Fatalf("explicit bypass = %v, %v", proxy, err)
	}
}
