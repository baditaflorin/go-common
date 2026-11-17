package proxysupplier

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func staticRouteConfig(t *testing.T, pools ...StaticRoutePoolConfig) RouteRouterConfig {
	t.Helper()
	return RouteRouterConfig{Providers: []RouteProviderConfig{{ID: "thunder", Type: RouteProviderStatic, Pools: pools}}}
}

func staticPoolFile(t *testing.T, id, class string, weight int) StaticRoutePoolConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), id+".proxy")
	if err := os.WriteFile(path, []byte("http://user:secret-"+id+"@proxy.example:3128\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return StaticRoutePoolConfig{ID: id, AccountID: "account-" + id, Class: PoolClass(class), ProxyURLFile: path, Weight: weight}
}

func TestRouteRouterStrictPinAndOrderedProfile(t *testing.T) {
	dc := staticPoolFile(t, "dc", string(PoolClassDC), 2)
	residential := staticPoolFile(t, "residential", string(PoolClassResidential), 1)
	cfg := staticRouteConfig(t, dc, residential)
	cfg.Profiles = []RouteProfileConfig{{ID: "fallback", Candidates: []RouteCandidateRef{
		{ProviderID: "thunder", PoolID: "dc"},
		{ProviderID: "thunder", PoolID: "residential"},
	}}}
	router, err := NewRouteRouter(cfg)
	if err != nil {
		t.Fatal("NewRouteRouter:", err)
	}
	t.Cleanup(router.Close)

	chain, err := router.ResolveCandidates(RouteIntent{ProfileID: "fallback"})
	if err != nil {
		t.Fatal("resolve profile:", err)
	}
	if len(chain) != 2 || chain[0].PoolID != "dc" || chain[1].PoolID != "residential" || chain[0].ProfileID != "fallback" {
		t.Fatalf("profile candidates = %#v, want exact ordered chain", chain)
	}
	if _, err := router.ResolveCandidates(RouteIntent{PoolID: "missing"}); err == nil {
		t.Fatal("unknown pool pin unexpectedly resolved")
	}
	if _, err := router.ResolveCandidates(RouteIntent{ProviderID: "missing"}); err == nil {
		t.Fatal("unknown provider pin unexpectedly resolved")
	}
	if _, err := router.ResolveCandidates(RouteIntent{ProfileID: "fallback", ProviderID: "thunder"}); err == nil {
		t.Fatal("conflicting selectors unexpectedly resolved")
	}
	if _, err := router.SelectRoute(chain[1], false); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("residential selection without permit = %v, want unavailable", err)
	}
	selection, err := router.SelectRoute(chain[1], true)
	if err != nil {
		t.Fatal("residential selection with permit:", err)
	}
	if selection.Descriptor().PoolID != "residential" {
		t.Fatalf("selected descriptor = %#v", selection.Descriptor())
	}
	formatted := fmt.Sprintf("%v %#v", selection, selection)
	if strings.Contains(formatted, "secret-residential") || strings.Contains(formatted, "proxy.example") {
		t.Fatalf("formatted route selection leaked endpoint: %s", formatted)
	}
	proxyURL, err := selection.ProxyURL()
	password := ""
	if err == nil && proxyURL.User != nil {
		password, _ = proxyURL.User.Password()
	}
	if err != nil || password != "secret-residential" {
		t.Fatalf("credential-bearing proxy URL was not available (err=%v)", err)
	}
	router.MarkRouteResult(selection, RouteOutcomeTransportFailure)
	selection.Report(RouteOutcomeOriginResponse)
	if _, err := router.SelectRoute(RouteCandidate{ProviderID: "thunder", PoolID: "dc", AccountID: "account-dc", Class: PoolClassDC}, false); err != nil {
		t.Fatalf("exact DC selection = %v", err)
	}
}

func TestRouteRouterProviderClassUsesFairPoolOrder(t *testing.T) {
	router, err := NewRouteRouter(staticRouteConfig(t,
		staticPoolFile(t, "large", string(PoolClassDC), 2),
		staticPoolFile(t, "small", string(PoolClassDC), 1),
		staticPoolFile(t, "residential", string(PoolClassResidential), 1),
	))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(router.Close)
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		candidates, err := router.ResolveCandidates(RouteIntent{ProviderID: "thunder"})
		if err != nil {
			t.Fatal("resolve provider/class:", err)
		}
		if len(candidates) != 2 || candidates[0].Class != PoolClassDC || candidates[1].Class != PoolClassDC {
			t.Fatalf("provider-only default candidates = %#v, want only DC", candidates)
		}
		counts[candidates[0].PoolID]++
	}
	if counts["large"] != 200 || counts["small"] != 100 {
		t.Fatalf("weighted first-candidate shares = %#v, want 2:1", counts)
	}
}

func TestParseRouteIntentQuery(t *testing.T) {
	cases := []struct {
		name string
		q    url.Values
		want RouteIntent
		bad  bool
	}{
		{name: "pool", q: url.Values{"proxy_pool": {"quality-dc"}}, want: RouteIntent{PoolID: "quality-dc"}},
		{name: "provider and class", q: url.Values{"proxy_provider": {"webshare"}, "proxy_class": {"dc"}}, want: RouteIntent{ProviderID: "webshare", Class: PoolClassDC}},
		{name: "profile", q: url.Values{"proxy_profile": {"ordered"}}, want: RouteIntent{ProfileID: "ordered"}},
		{name: "conflicting selectors", q: url.Values{"proxy_pool": {"pool"}, "proxy_provider": {"provider"}}, bad: true},
		{name: "class without provider", q: url.Values{"proxy_class": {"residential"}}, bad: true},
		{name: "unknown class", q: url.Values{"proxy_provider": {"provider"}, "proxy_class": {"unknown"}}, bad: true},
		{name: "duplicate selector", q: url.Values{"proxy_pool": {"one", "two"}}, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRouteIntentQuery(tc.q)
			if tc.bad {
				if err == nil {
					t.Fatalf("ParseRouteIntentQuery(%v) unexpectedly succeeded", tc.q)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParseRouteIntentQuery(%v) = %#v, %v; want %#v", tc.q, got, err, tc.want)
			}
		})
	}
}

func TestRouteRouterConfigRejectsUnknownFieldsAndDuplicateCandidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(path, []byte(`{"providers":[],"unknown":"value"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRouteRouterConfig(path); err == nil {
		t.Fatal("unknown route config field was accepted")
	}
	pool := staticPoolFile(t, "dc", string(PoolClassDC), 1)
	cfg := staticRouteConfig(t, pool)
	cfg.Profiles = []RouteProfileConfig{{ID: "dupe", Candidates: []RouteCandidateRef{
		{ProviderID: "thunder", PoolID: "dc"}, {ProviderID: "thunder", PoolID: "dc"},
	}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRouteRouterFromFile(path); err == nil {
		t.Fatal("duplicate profile candidate was accepted")
	}
}

func TestWebshareRouteFeedbackIsBoundToExactAccountEntry(t *testing.T) {
	var oldRequests, qualityRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("plan_id") {
		case "1001":
			oldRequests.Add(1)
		case "2002":
			qualityRequests.Add(1)
		default:
			t.Errorf("unexpected plan id %q", r.URL.Query().Get("plan_id"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(webshareListResponse{Results: []webshareEntry{{
			Username: "proxy", Password: "pass", ProxyAddress: "192.0.2.88", Port: 8000, Valid: true,
		}}})
	}))
	defer server.Close()
	withWebshareDirectBaseURL(t, server.URL)
	previousDefault := activeWebshareMultiSupplier.Load()
	defaultPool := &webshareDirectSupplier{}
	installEntries(defaultPool, testEntry("192.0.2.50", 8000, "default"))
	defaultMulti := &webshareMultiSupplier{pools: []*webshareNamedPool{{
		WebsharePoolConfig: WebsharePoolConfig{ID: "process-default", AccountID: "default-account", Class: PoolClassDC},
		supplier:           defaultPool,
	}}}
	activeWebshareMultiSupplier.Store(defaultMulti)
	t.Cleanup(func() { activeWebshareMultiSupplier.Store(previousDefault) })
	dir := t.TempDir()
	poolsPath := writePoolConfig(t,
		poolConfig(t, dir, "legacy", "old-account", PoolClassDC, "1001", 0),
		poolConfig(t, dir, "quality", "new-account", PoolClassDC, "2002", 0),
	)
	router, err := NewRouteRouter(RouteRouterConfig{
		Providers: []RouteProviderConfig{{ID: "webshare", Type: RouteProviderWebshare, PoolsFile: poolsPath}},
		Profiles:  []RouteProfileConfig{{ID: "quality-profile", Candidates: []RouteCandidateRef{{ProviderID: "webshare", PoolID: "quality"}}}},
	})
	if err != nil {
		t.Fatal("NewRouteRouter:", err)
	}
	t.Cleanup(router.Close)
	if snapshot := WebsharePoolsSnapshot(nil); len(snapshot) != 1 || snapshot[0].PoolID != "process-default" {
		t.Fatalf("route router stole process-default Webshare snapshot: %#v", snapshot)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (oldRequests.Load() == 0 || qualityRequests.Load() == 0) {
		time.Sleep(time.Millisecond)
	}
	if oldRequests.Load() == 0 || qualityRequests.Load() == 0 {
		t.Fatalf("both account catalogs were not requested: old=%d quality=%d", oldRequests.Load(), qualityRequests.Load())
	}
	var candidates []RouteCandidate
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		candidates, err = router.ResolveCandidates(RouteIntent{ProviderID: "webshare", Class: PoolClassDC})
		if err == nil && len(candidates) == 2 {
			ready := true
			for _, candidate := range candidates {
				if _, selectErr := router.SelectRoute(candidate, false); selectErr != nil {
					ready = false
				}
			}
			if ready {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil || len(candidates) != 2 {
		t.Fatalf("resolve Webshare candidates: %#v err=%v", candidates, err)
	}
	profileCandidates, err := router.ResolveCandidates(RouteIntent{ProfileID: "quality-profile"})
	if err != nil || len(profileCandidates) != 1 || profileCandidates[0].PoolID != "quality" {
		t.Fatalf("resolve Webshare profile: %#v err=%v", profileCandidates, err)
	}
	var oldCandidate, qualityCandidate RouteCandidate
	for _, candidate := range candidates {
		if candidate.PoolID == "legacy" {
			oldCandidate = candidate
		}
		if candidate.PoolID == "quality" {
			qualityCandidate = candidate
		}
	}
	if oldCandidate.PoolID == "" || qualityCandidate.PoolID == "" {
		t.Fatalf("missing configured candidates: %#v", candidates)
	}
	var selected RouteSelection
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		selected, err = router.SelectRoute(qualityCandidate, false)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal("select quality route:", err)
	}
	selected.Report(RouteOutcomeChallenge)
	selected.Report(RouteOutcomeCanceled)
	if _, err := router.SelectRoute(qualityCandidate, false); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("challenge feedback did not cool selected account route: %v", err)
	}
	if _, err := router.SelectRoute(oldCandidate, false); err != nil {
		t.Fatalf("feedback for overlapping address penalized legacy account: %v", err)
	}
	if selected.Descriptor().ProviderID != "webshare" || selected.Descriptor().AccountID != "new-account" {
		t.Fatalf("route receipt has wrong safe identity: %#v", selected.Descriptor())
	}
	if snapshot := WebsharePoolsSnapshot(nil); len(snapshot) != 1 || snapshot[0].PoolID != "process-default" {
		t.Fatalf("explicit route changed default supplier snapshot: %#v", snapshot)
	}
}
