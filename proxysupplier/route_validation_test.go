package proxysupplier

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRouteRouterConfigAndEnvironmentConstructor(t *testing.T) {
	pool := staticPoolFile(t, "dc", string(PoolClassDC), 1)
	cfg := staticRouteConfig(t, pool)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal("marshal route config:", err)
	}
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal("write route config:", err)
	}
	loaded, err := LoadRouteRouterConfig(path)
	if err != nil || len(loaded.Providers) != 1 || loaded.Providers[0].ID != "thunder" {
		t.Fatalf("LoadRouteRouterConfig = %#v, err=%v", loaded, err)
	}
	router, err := NewRouteRouterFromFile(path)
	if err != nil {
		t.Fatal("NewRouteRouterFromFile:", err)
	}
	router.Close()
	router.Close()

	t.Setenv(EnvRouteRouterFile, "")
	if got, err := NewRouteRouterFromEnv(); err != nil || got != nil {
		t.Fatalf("unset env constructor = (%v,%v), want nil,nil", got, err)
	}
	t.Setenv(EnvRouteRouterFile, path)
	router, err = NewRouteRouterFromEnv()
	if err != nil || router == nil {
		t.Fatalf("configured env constructor = (%v,%v), want router", router, err)
	}
	router.Close()
}

func TestLoadRouteRouterConfigRejectsMalformedBoundedFiles(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		data []byte
	}{
		{name: "empty"},
		{name: "malformed JSON", data: []byte("{")},
		{name: "trailing JSON", data: []byte(`{"providers":[{"id":"p","type":"static_proxy","pools":[{"id":"x","account_id":"a","class":"dc","proxy_url_file":"/run/x"}]}]} {}`)},
		{name: "unknown fields", data: []byte(`{"providers":[],"inline_proxy_url":"http://user:secret@proxy"}`)},
		{name: "oversize", data: make([]byte, maxRouteConfigBytes+1)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("bad-%d.json", i))
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal("write:", err)
			}
			if _, err := LoadRouteRouterConfig(path); err == nil {
				t.Fatal("invalid config unexpectedly loaded")
			} else if strings.Contains(err.Error(), "secret") {
				t.Fatalf("config error leaked inline value: %v", err)
			}
		})
	}
	if _, err := LoadRouteRouterConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing route config unexpectedly loaded")
	}
	if _, err := LoadRouteRouterConfig(dir); err == nil {
		t.Fatal("directory route config unexpectedly loaded")
	}
}

func TestRouteRouterRejectsInvalidIntentsAndExactCandidates(t *testing.T) {
	dc := staticPoolFile(t, "dc", string(PoolClassDC), 1)
	res := staticPoolFile(t, "res", string(PoolClassResidential), 1)
	duplicatePool := staticPoolFile(t, "dc", string(PoolClassDC), 1)
	cfg := RouteRouterConfig{Providers: []RouteProviderConfig{
		{ID: "thunder", Type: RouteProviderStatic, Pools: []StaticRoutePoolConfig{dc, res}},
		{ID: "other", Type: RouteProviderStatic, Pools: []StaticRoutePoolConfig{duplicatePool}},
	}}
	router, err := NewRouteRouter(cfg)
	if err != nil {
		t.Fatal("NewRouteRouter:", err)
	}
	t.Cleanup(router.Close)
	cases := []struct {
		name   string
		intent RouteIntent
	}{
		{name: "empty"},
		{name: "invalid class", intent: RouteIntent{ProviderID: "thunder", Class: PoolClass("unknown")}},
		{name: "profile not found", intent: RouteIntent{ProfileID: "missing"}},
		{name: "profile class conflict", intent: RouteIntent{ProfileID: "missing", Class: PoolClassDC}},
		{name: "pool class conflict", intent: RouteIntent{PoolID: "dc", Class: PoolClassDC}},
		{name: "unknown pool", intent: RouteIntent{PoolID: "missing"}},
		{name: "ambiguous pool", intent: RouteIntent{PoolID: "dc"}},
		{name: "unknown provider", intent: RouteIntent{ProviderID: "missing"}},
		{name: "class unavailable", intent: RouteIntent{ProviderID: "other", Class: PoolClassResidential}},
		{name: "unsafe ID", intent: RouteIntent{PoolID: "../dc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := router.ResolveCandidates(tc.intent); err == nil {
				t.Fatalf("ResolveCandidates(%#v) unexpectedly succeeded", tc.intent)
			}
		})
	}
	candidates, err := router.ResolveCandidates(RouteIntent{ProviderID: "thunder", Class: PoolClassDC})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("valid DC candidates = %#v, err=%v", candidates, err)
	}
	wrong := candidates[0]
	wrong.AccountID = "wrong-account"
	for name, candidate := range map[string]RouteCandidate{
		"wrong account":    wrong,
		"wrong class":      {ProviderID: "thunder", PoolID: "dc", AccountID: "account-dc", Class: PoolClassResidential},
		"unknown provider": {ProviderID: "missing", PoolID: "dc", AccountID: "account-dc", Class: PoolClassDC},
	} {
		t.Run("candidate "+name, func(t *testing.T) {
			if _, err := router.SelectRoute(candidate, false); !errors.Is(err, ErrProxyUnavailable) {
				t.Fatalf("SelectRoute error = %v, want ErrProxyUnavailable", err)
			}
		})
	}
	residential, err := router.ResolveCandidates(RouteIntent{ProviderID: "thunder", Class: PoolClassResidential})
	if err != nil || len(residential) != 1 {
		t.Fatalf("valid residential descriptor = %#v, err=%v", residential, err)
	}
	forgedResidential := residential[0]
	forgedResidential.Class = PoolClassDC
	if _, err := router.SelectRoute(forgedResidential, false); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("residential class forgery bypassed permit: %v", err)
	}
	selection, err := router.SelectRoute(candidates[0], false)
	if err != nil {
		t.Fatal("valid selection:", err)
	}
	other, err := NewRouteRouter(staticRouteConfig(t, staticPoolFile(t, "other", string(PoolClassDC), 1)))
	if err != nil {
		t.Fatal("other router:", err)
	}
	other.MarkRouteResult(selection, RouteOutcomeTransportFailure)
	other.MarkRouteResult(selection, RouteOutcome(255))
	if _, err := selection.ProxyURL(); err != nil {
		t.Fatalf("foreign feedback invalidated route: %v", err)
	}
	other.Close()
	router.Close()
	if _, err := router.ResolveCandidates(RouteIntent{PoolID: "dc"}); err == nil {
		t.Fatal("closed router still resolved candidates")
	}
	if _, err := router.SelectRoute(candidates[0], false); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("closed router selected route: %v", err)
	}
	if _, err := ParseRouteIntentQuery(url.Values{"proxy_pool": {"../secret"}}); err == nil {
		t.Fatal("unsafe query selector was accepted")
	}
}

func TestRouteRouterConfigValidationFailsClosed(t *testing.T) {
	validPool := StaticRoutePoolConfig{ID: "pool", AccountID: "account", Class: PoolClassDC, ProxyURLFile: "/run/secret.proxy"}
	base := RouteRouterConfig{Providers: []RouteProviderConfig{{ID: "provider", Type: RouteProviderStatic, Pools: []StaticRoutePoolConfig{validPool}}}}
	cases := []struct {
		name string
		mut  func(*RouteRouterConfig)
	}{
		{name: "no providers", mut: func(c *RouteRouterConfig) { c.Providers = nil }},
		{name: "unsafe provider ID", mut: func(c *RouteRouterConfig) { c.Providers[0].ID = "../provider" }},
		{name: "unknown type", mut: func(c *RouteRouterConfig) { c.Providers[0].Type = "unknown" }},
		{name: "webshare missing pools file", mut: func(c *RouteRouterConfig) {
			c.Providers[0] = RouteProviderConfig{ID: "provider", Type: RouteProviderWebshare}
		}},
		{name: "webshare relative file", mut: func(c *RouteRouterConfig) {
			c.Providers[0] = RouteProviderConfig{ID: "provider", Type: RouteProviderWebshare, PoolsFile: "relative.json"}
		}},
		{name: "webshare plus static pools", mut: func(c *RouteRouterConfig) {
			c.Providers[0].Type = RouteProviderWebshare
			c.Providers[0].PoolsFile = "/run/pools.json"
		}},
		{name: "static plus pools file", mut: func(c *RouteRouterConfig) { c.Providers[0].PoolsFile = "/run/unused.json" }},
		{name: "duplicate providers", mut: func(c *RouteRouterConfig) { c.Providers = append(c.Providers, c.Providers[0]) }},
		{name: "empty static pools", mut: func(c *RouteRouterConfig) { c.Providers[0].Pools = nil }},
		{name: "unsafe pool ID", mut: func(c *RouteRouterConfig) { c.Providers[0].Pools[0].ID = "../pool" }},
		{name: "unsafe account ID", mut: func(c *RouteRouterConfig) { c.Providers[0].Pools[0].AccountID = "../account" }},
		{name: "invalid class", mut: func(c *RouteRouterConfig) { c.Providers[0].Pools[0].Class = "unknown" }},
		{name: "relative endpoint file", mut: func(c *RouteRouterConfig) { c.Providers[0].Pools[0].ProxyURLFile = "proxy" }},
		{name: "negative weight", mut: func(c *RouteRouterConfig) { c.Providers[0].Pools[0].Weight = -1 }},
		{name: "duplicate pools", mut: func(c *RouteRouterConfig) {
			c.Providers[0].Pools = append(c.Providers[0].Pools, c.Providers[0].Pools[0])
		}},
		{name: "invalid profile ID", mut: func(c *RouteRouterConfig) {
			c.Profiles = []RouteProfileConfig{{Candidates: []RouteCandidateRef{{ProviderID: "provider", PoolID: "pool"}}}}
		}},
		{name: "empty profile", mut: func(c *RouteRouterConfig) { c.Profiles = []RouteProfileConfig{{ID: "empty"}} }},
		{name: "unknown profile provider", mut: func(c *RouteRouterConfig) {
			c.Profiles = []RouteProfileConfig{{ID: "bad", Candidates: []RouteCandidateRef{{ProviderID: "missing", PoolID: "pool"}}}}
		}},
		{name: "unknown profile pool", mut: func(c *RouteRouterConfig) {
			c.Profiles = []RouteProfileConfig{{ID: "bad", Candidates: []RouteCandidateRef{{ProviderID: "provider", PoolID: "missing"}}}}
		}},
		{name: "duplicate profile candidates", mut: func(c *RouteRouterConfig) {
			ref := RouteCandidateRef{ProviderID: "provider", PoolID: "pool"}
			c.Profiles = []RouteProfileConfig{{ID: "bad", Candidates: []RouteCandidateRef{ref, ref}}}
		}},
		{name: "duplicate profile IDs", mut: func(c *RouteRouterConfig) {
			c.Profiles = []RouteProfileConfig{{ID: "same", Candidates: []RouteCandidateRef{{ProviderID: "provider", PoolID: "pool"}}}, {ID: "same", Candidates: []RouteCandidateRef{{ProviderID: "provider", PoolID: "pool"}}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Providers = append([]RouteProviderConfig(nil), base.Providers...)
			cfg.Providers[0].Pools = append([]StaticRoutePoolConfig(nil), base.Providers[0].Pools...)
			tc.mut(&cfg)
			if err := validateRouteRouterConfig(cfg); err == nil {
				t.Fatal("invalid route configuration was accepted")
			}
		})
	}
}

func TestWebshareProfileReferencesValidateWithoutOpeningKeyFiles(t *testing.T) {
	poolsPath := filepath.Join(t.TempDir(), "webshare-pools.json")
	data, err := json.Marshal(WebsharePoolsConfig{Pools: []WebsharePoolConfig{{
		ID: "configured", AccountID: "account", Class: PoolClassDC,
		PlanID: "12345", APIKeyFile: filepath.Join(t.TempDir(), "missing.key"),
	}}})
	if err != nil {
		t.Fatal("marshal Webshare pools:", err)
	}
	if err := os.WriteFile(poolsPath, data, 0600); err != nil {
		t.Fatal("write Webshare pools:", err)
	}
	_, err = NewRouteRouter(RouteRouterConfig{
		Providers: []RouteProviderConfig{{ID: "webshare", Type: RouteProviderWebshare, PoolsFile: poolsPath}},
		Profiles:  []RouteProfileConfig{{ID: "bad-profile", Candidates: []RouteCandidateRef{{ProviderID: "webshare", PoolID: "missing"}}}},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown candidate") {
		t.Fatalf("profile validation error = %v, want unknown candidate before key-file open", err)
	}
}

func TestStaticRoutePortDefaults(t *testing.T) {
	for raw, want := range map[string]string{
		"http://proxy.example":      "80",
		"https://proxy.example":     "443",
		"http://proxy.example:3128": "3128",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse test URL: %v", err)
		}
		if got, err := strconvPort(u); err != nil || got != want {
			t.Fatalf("strconvPort(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := strconvPort(&url.URL{Scheme: "http"}); err == nil {
		t.Fatal("missing static endpoint host unexpectedly accepted")
	}
}

func TestStaticRouteProviderRejectsInvalidCredentialFilesWithoutLeaks(t *testing.T) {
	for name, contents := range map[string]string{
		"empty":              "",
		"invalid URL":        "http://user:secret@bad host",
		"fragment":           "http://proxy.example:80/#secret",
		"path":               "http://proxy.example:80/path",
		"query":              "http://proxy.example:80/?secret=value",
		"unsupported scheme": "socks5://proxy.example:1080",
		"invalid port":       "http://proxy.example:notaport",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxy.url")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal("write:", err)
			}
			cfg := staticRouteConfig(t, StaticRoutePoolConfig{ID: "pool", AccountID: "account", Class: PoolClassDC, ProxyURLFile: path})
			_, err := NewRouteRouter(cfg)
			if err == nil {
				t.Fatal("invalid static endpoint unexpectedly accepted")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "proxy.example") {
				t.Fatalf("route error leaked endpoint content: %v", err)
			}
		})
	}
	if _, err := readSecretFile(filepath.Join(t.TempDir(), "missing"), 16); err == nil {
		t.Fatal("missing endpoint file unexpectedly read")
	}
	if _, err := readSecretFile(t.TempDir(), 16); err == nil {
		t.Fatal("directory endpoint file unexpectedly read")
	}
}
