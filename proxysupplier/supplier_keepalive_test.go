package proxysupplier_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/proxysupplier"
)

// TestHTTPClient_KeepAliveDefaultsToDisabled is the backward-compatibility
// guarantee: every existing caller (constructing Config{} directly, or via
// EnvConfig() before PROXY_KEEPALIVE_ENABLED existed) must keep getting
// exactly today's behavior -- a fresh proxy connection per request -- with
// zero code changes required on their part.
func TestHTTPClient_KeepAliveDefaultsToDisabled(t *testing.T) {
	for _, supplierKind := range []string{"env", "plain_proxies"} {
		t.Run(supplierKind, func(t *testing.T) {
			cfg := proxysupplier.Config{
				Supplier:         supplierKind,
				ExternalProxyURL: "http://proxy.example.com:8080/",
				Host:             "proxy.example.com",
				Port:             "8080",
			}
			s := proxysupplier.NewFromConfig(cfg)
			client := proxysupplier.HTTPClient(s, 2*time.Second)
			tr, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatal("expected *http.Transport")
			}
			if !tr.DisableKeepAlives {
				t.Error("DisableKeepAlives = false, want true (default/unset KeepAliveEnabled must preserve the original fresh-connection-per-request behavior)")
			}
		})
	}
}

// TestHTTPClient_KeepAliveEnabledOptsIn is the fix itself: a caller that
// explicitly sets Config.KeepAliveEnabled gets a Transport that reuses
// connections. Regression target: go-search-duck-go's DDG requests were
// failing with "context deadline exceeded" against an already-congested
// Webshare gateway because every single request re-paid a full TCP+TLS+
// CONNECT handshake -- this is the knob that lets that service (and any
// other PROXY_SUPPLIER=env/multi consumer) opt out of that cost.
func TestHTTPClient_KeepAliveEnabledOptsIn(t *testing.T) {
	for _, supplierKind := range []string{"env", "plain_proxies", "multi"} {
		t.Run(supplierKind, func(t *testing.T) {
			cfg := proxysupplier.Config{
				Supplier:         supplierKind,
				ExternalProxyURL: "http://proxy.example.com:8080/",
				Host:             "proxy.example.com",
				Port:             "8080",
				ProxyURLs:        "http://proxy.example.com:8080/",
				KeepAliveEnabled: true,
			}
			s := proxysupplier.NewFromConfig(cfg)
			client := proxysupplier.HTTPClient(s, 2*time.Second)
			tr, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatal("expected *http.Transport")
			}
			if tr.DisableKeepAlives {
				t.Error("DisableKeepAlives = true, want false (Config.KeepAliveEnabled=true should reuse connections)")
			}
		})
	}
}

// TestEnvConfig_ReadsKeepAliveEnabled covers the env-var wiring specifically
// (case-insensitivity and the "unset/anything else means false" default),
// separate from the Transport-level assertions above.
func TestEnvConfig_ReadsKeepAliveEnabled(t *testing.T) {
	cases := []struct {
		envVal string
		want   bool
	}{
		{"", false},
		{"false", false},
		{"0", false},
		{"garbage", false},
		{"true", true},
		{"TRUE", true},
		{"True", true},
	}
	for _, c := range cases {
		t.Run("value="+c.envVal, func(t *testing.T) {
			t.Setenv("PROXY_KEEPALIVE_ENABLED", c.envVal)
			cfg := proxysupplier.EnvConfig()
			if cfg.KeepAliveEnabled != c.want {
				t.Errorf("PROXY_KEEPALIVE_ENABLED=%q -> KeepAliveEnabled = %v, want %v", c.envVal, cfg.KeepAliveEnabled, c.want)
			}
		})
	}
}
