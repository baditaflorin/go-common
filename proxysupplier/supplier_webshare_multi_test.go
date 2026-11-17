package proxysupplier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writePoolConfig(t *testing.T, pools ...WebsharePoolConfig) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range pools {
		if err := os.WriteFile(p.APIKeyFile, []byte("key-"+p.ID+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(WebsharePoolsConfig{Pools: pools})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pools.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func poolConfig(t *testing.T, dir, id, account string, class PoolClass, plan string, weight int) WebsharePoolConfig {
	t.Helper()
	keyPath := filepath.Join(dir, id+".key")
	if err := os.WriteFile(keyPath, []byte("key-"+id+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return WebsharePoolConfig{ID: id, AccountID: account, Class: class, PlanID: plan, APIKeyFile: keyPath, Weight: weight}
}

func installEntries(s *webshareDirectSupplier, entries ...*webshareDirectEntry) {
	s.mu.Lock()
	s.list = entries
	s.mu.Unlock()
}

func testEntry(address string, port int, user string) *webshareDirectEntry {
	addr := net.JoinHostPort(address, strconv.Itoa(port))
	return &webshareDirectEntry{addr: addr, proxyURL: "http://" + user + ":pass@" + addr}
}

func testMulti(pools ...*webshareNamedPool) *webshareMultiSupplier {
	return &webshareMultiSupplier{pools: pools, rules: parseNoProxy("")}
}

func directSupplierForTest(apiKey, planID string) *webshareDirectSupplier {
	return &webshareDirectSupplier{
		apiKey: apiKey, planID: planID, baseURL: webshareDirectDefaultBaseURL,
		refreshInterval: webshareDirectDefaultRefreshInterval,
		httpClient:      &http.Client{Timeout: webshareDirectDefaultHTTPTimeout},
		rules:           parseNoProxy(""), stop: make(chan struct{}),
		retryBase: webshareDirectRetryBase, retryMax: webshareDirectRetryMax,
	}
}

func TestWebshareMultiFetchesSeparatePlanIDsAndAccountHeaders(t *testing.T) {
	var seenLegacy, seenQuality atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plan := r.URL.Query().Get("plan_id")
		wantKey := ""
		switch plan {
		case "1001":
			wantKey = "Token key-legacy"
			seenLegacy.Add(1)
		case "2002":
			wantKey = "Token key-quality"
			seenQuality.Add(1)
		default:
			t.Errorf("unexpected plan_id %q", plan)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); got != wantKey {
			t.Errorf("Authorization = %q, want expected account key", got)
		}
		_ = json.NewEncoder(w).Encode(webshareListResponse{Results: []webshareEntry{{Username: "proxy", Password: "pass", ProxyAddress: "192.0.2.10", Port: 8000, Valid: true}}})
	}))
	defer server.Close()
	withWebshareDirectBaseURL(t, server.URL)
	dir := t.TempDir()
	path := writePoolConfig(t,
		poolConfig(t, dir, "legacy", "old-account", PoolClassDC, "1001", 0),
		poolConfig(t, dir, "quality", "new-account", PoolClassDC, "2002", 0),
	)
	supplier := newWebshareMultiSupplier(path, parseNoProxy(""))
	m, ok := supplier.(*webshareMultiSupplier)
	if !ok {
		t.Fatalf("supplier type = %T, want webshare_multi", supplier)
	}
	t.Cleanup(m.Close)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if seenLegacy.Load() > 0 && seenQuality.Load() > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if seenLegacy.Load() == 0 || seenQuality.Load() == 0 {
		t.Fatalf("plan catalogs not both requested: legacy=%d quality=%d", seenLegacy.Load(), seenQuality.Load())
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pools := m.Pools()
		if len(pools) == 2 && pools[0].Total+pools[1].Total == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	selection, err := m.SelectPool(PoolClassDC)
	if err != nil {
		t.Fatal("SelectPool(DC):", err)
	}
	if selection.Class != PoolClassDC || selection.PoolID == "" {
		t.Fatalf("selection lacks pool metadata: %s", selection)
	}
	formatted := fmt.Sprintf("%v %#v", selection, selection)
	if strings.Contains(formatted, "key-") || strings.Contains(formatted, "pass") || strings.Contains(formatted, "192.0.2.10") {
		t.Fatal("selection formatting exposed proxy credentials or exit address")
	}
}

func TestWebshareMultiDefaultExcludesResidentialAndFailsClosed(t *testing.T) {
	res := &webshareDirectSupplier{}
	installEntries(res, testEntry("192.0.2.10", 8000, "isp"))
	m := testMulti(&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "isp", AccountID: "account", Class: PoolClassResidential}, supplier: res})
	if got := m.ProxyURL(); got != "" {
		t.Fatalf("default ProxyURL selected residential pool: %q", got)
	}
	if _, err := m.SelectPool(PoolClassDC); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("DC selection error = %v, want ErrProxyUnavailable", err)
	}
	client := HTTPClient(m, time.Second)
	resp, err := client.Get("http://127.0.0.1:1/should-not-connect")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("HTTPClient error = %v, want fail-closed ErrProxyUnavailable", err)
	}
}

func TestWebshareMultiDefaultPoolCanPinNamedDCOnly(t *testing.T) {
	legacy := &webshareDirectSupplier{}
	quality := &webshareDirectSupplier{}
	installEntries(legacy, testEntry("192.0.2.10", 8000, "legacy"))
	installEntries(quality, testEntry("192.0.2.11", 8000, "quality"))
	res := &webshareDirectSupplier{}
	installEntries(res, testEntry("192.0.2.12", 8000, "isp"))
	m := testMulti(
		&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "legacy", AccountID: "legacy-account", Class: PoolClassDC}, supplier: legacy},
		&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "quality", AccountID: "quality-account", Class: PoolClassDC}, supplier: quality},
		&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "isp", AccountID: "quality-account", Class: PoolClassResidential}, supplier: res},
	)
	m.defaultPoolID = "quality"
	selection, err := m.SelectDefaultPool()
	if err != nil {
		t.Fatal(err)
	}
	if selection.PoolID != "quality" || selection.Class != PoolClassDC {
		t.Fatalf("default selection = %s, want pinned quality DC", selection)
	}
	if got := m.ProxyURL(); !strings.Contains(got, "192.0.2.11:8000") {
		t.Fatalf("ProxyURL did not honor named DC default: %q", got)
	}
}

func TestWebshareMultiRejectsInvalidDefaultPoolWithoutFallback(t *testing.T) {
	dir := t.TempDir()
	path := writePoolConfig(t,
		poolConfig(t, dir, "legacy", "legacy-account", PoolClassDC, "1001", 0),
		poolConfig(t, dir, "isp", "quality-account", PoolClassResidential, "2002", 0),
	)
	for _, id := range []string{"missing", "isp"} {
		supplier := NewFromConfig(Config{Supplier: "webshare_multi", WebsharePoolsFile: path, WebshareDefaultPoolID: id})
		if supplier.Name() != "webshare_multi" || supplier.ProxyURL() != "" {
			t.Fatalf("invalid default %q fell back: supplier=%q proxy=%q", id, supplier.Name(), supplier.ProxyURL())
		}
	}
}

func TestWebshareMultiDefaultSelectorFailsClosedForInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	validPath := writePoolConfig(t,
		poolConfig(t, dir, "legacy", "legacy-account", PoolClassDC, "1001", 0),
		poolConfig(t, dir, "isp", "quality-account", PoolClassResidential, "2002", 0),
	)
	cases := []struct {
		name    string
		path    string
		poolIDs []string
	}{
		{name: "missing config file", path: filepath.Join(t.TempDir(), "missing.json")},
		{name: "blank config path"},
		{name: "unsafe default pool id", path: validPath, poolIDs: []string{"../legacy"}},
		{name: "multiple default pool ids", path: validPath, poolIDs: []string{"legacy", "isp"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			supplier := newWebshareMultiSupplier(tc.path, parseNoProxy(""), tc.poolIDs...)
			if supplier.Name() != "webshare_multi" || supplier.ProxyURL() != "" {
				t.Fatalf("invalid selector configuration fell back: name=%q proxy=%q", supplier.Name(), supplier.ProxyURL())
			}
			client := HTTPClient(supplier, time.Second)
			resp, err := client.Get("http://127.0.0.1:1/should-not-connect")
			if resp != nil {
				resp.Body.Close()
			}
			if !errors.Is(err, ErrProxyUnavailable) {
				t.Fatalf("invalid selector configuration HTTP error = %v, want ErrProxyUnavailable", err)
			}
		})
	}
}

func TestWebshareMultiSelectPoolRejectsUnknownClassAndExcludedPools(t *testing.T) {
	dc := &webshareDirectSupplier{}
	installEntries(dc, testEntry("192.0.2.20", 8000, "dc"))
	m := testMulti(&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "dc", Class: PoolClassDC}, supplier: dc})
	if _, err := m.SelectPool(PoolClass("unknown")); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("unknown class selection error = %v, want ErrProxyUnavailable", err)
	}
	if _, err := m.SelectPool(PoolClassDC, "dc"); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("all-excluded selection error = %v, want ErrProxyUnavailable", err)
	}
}

func TestWebsharePoolInfoHelpersExposeOnlySafeConfiguredLabels(t *testing.T) {
	previous := activeWebshareMultiSupplier.Load()
	activeWebshareMultiSupplier.Store(nil)
	t.Cleanup(func() { activeWebshareMultiSupplier.Store(previous) })
	dir := t.TempDir()
	path := writePoolConfig(t,
		poolConfig(t, dir, "quality", "quality-account", PoolClassDC, "2002", 0),
		poolConfig(t, dir, "legacy", "legacy-account", PoolClassDC, "1001", 0),
	)
	info, err := LoadWebsharePoolInfo(path)
	if err != nil {
		t.Fatal("LoadWebsharePoolInfo:", err)
	}
	if len(info) != 2 || info[0].PoolID != "legacy" || info[1].PoolID != "quality" {
		t.Fatalf("safe pool labels are not sorted: %#v", info)
	}
	for _, pool := range info {
		if pool.AccountID == "" || pool.Class != PoolClassDC {
			t.Fatalf("safe pool metadata missing: %#v", pool)
		}
	}
	if got := WebsharePools(noneSupplier{}); got != nil {
		t.Fatalf("WebsharePools(non-Webshare supplier) = %#v, want nil", got)
	}
	if got := WebsharePoolsSnapshot(noneSupplier{}); got != nil {
		t.Fatalf("WebsharePoolsSnapshot(non-Webshare supplier) = %#v, want nil", got)
	}
	if got := WebsharePoolsSnapshot(nil); got != nil {
		t.Fatalf("WebsharePoolsSnapshot(nil without active supplier) = %#v, want nil", got)
	}
	if _, err := LoadWebsharePoolInfo(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("LoadWebsharePoolInfo(missing file) unexpectedly succeeded")
	}
}

func TestWebshareMultiInvalidKeyFileFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pools.json")
	config := WebsharePoolsConfig{Pools: []WebsharePoolConfig{{
		ID: "dc", AccountID: "account", Class: PoolClassDC, PlanID: "1001", APIKeyFile: t.TempDir(),
	}}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	supplier := NewFromConfig(Config{Supplier: "webshare_multi", WebsharePoolsFile: path})
	if supplier.Name() != "webshare_multi" || supplier.ProxyURL() != "" {
		t.Fatalf("invalid pool config should remain fail-closed: name=%q proxy=%q", supplier.Name(), supplier.ProxyURL())
	}
	client := HTTPClient(supplier, time.Second)
	resp, err := client.Get("http://127.0.0.1:1/should-not-connect")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("invalid pool config HTTP error = %v, want ErrProxyUnavailable", err)
	}
}

func TestWebshareMultiCapacityWeightedSmoothRotation(t *testing.T) {
	legacy := &webshareDirectSupplier{}
	quality := &webshareDirectSupplier{}
	installEntries(legacy, testEntry("192.0.2.1", 8000, "l1"), testEntry("192.0.2.2", 8000, "l2"))
	installEntries(quality, testEntry("192.0.2.3", 8000, "q1"))
	m := testMulti(
		&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "legacy", AccountID: "acct", Class: PoolClassDC}, supplier: legacy},
		&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "quality", AccountID: "acct", Class: PoolClassDC}, supplier: quality},
	)
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		sel, err := m.SelectPool(PoolClassDC)
		if err != nil {
			t.Fatal(err)
		}
		counts[sel.PoolID]++
	}
	if counts["legacy"] != 200 || counts["quality"] != 100 {
		t.Fatalf("capacity-weighted shares = %#v, want 2:1", counts)
	}
	if got := m.ProxyURL(); got == "" {
		t.Fatal("compatibility ProxyURL returned empty with eligible DC entries")
	}
	if _, err := m.SelectPoolID("missing"); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("missing pool error = %v, want ErrProxyUnavailable", err)
	}
	if _, err := m.SelectPool("invalid-class"); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("invalid class error = %v, want ErrProxyUnavailable", err)
	}
}

func TestWebshareMultiSelectionSkipsExcludedAndUnavailablePools(t *testing.T) {
	cooled := &webshareDirectSupplier{}
	available := &webshareDirectSupplier{}
	installEntries(cooled, testEntry("192.0.2.21", 8000, "cooled"))
	installEntries(available, testEntry("192.0.2.22", 8000, "available"))
	cooled.MarkResult("192.0.2.21:8000", false)
	m := testMulti(
		&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "cooled", Class: PoolClassDC}, supplier: cooled},
		&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "available", Class: PoolClassDC}, supplier: available},
	)
	if _, err := m.SelectPoolID("cooled"); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("cooled pool selection error = %v, want ErrProxyUnavailable", err)
	}
	selection, err := m.SelectPool(PoolClassDC, "cooled")
	if err != nil || selection.PoolID != "available" {
		t.Fatalf("selection with excluded pool = %+v, %v", selection, err)
	}
	if _, err := m.SelectPool(PoolClassDC, "cooled", "available"); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("all-excluded selection error = %v, want ErrProxyUnavailable", err)
	}
}

func TestWebshareMultiFeedbackUsesExactPoolWhenAddressOverlaps(t *testing.T) {
	legacy := &webshareDirectSupplier{}
	quality := &webshareDirectSupplier{}
	installEntries(legacy, testEntry("192.0.2.1", 8000, "legacy"))
	installEntries(quality, testEntry("192.0.2.1", 8000, "quality"))
	m := testMulti(
		&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "legacy", AccountID: "acct", Class: PoolClassDC}, supplier: legacy},
		&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "quality", AccountID: "acct", Class: PoolClassDC}, supplier: quality},
	)
	sel, err := m.SelectPoolID("quality")
	if err != nil {
		t.Fatal(err)
	}
	m.MarkSelectionResult(sel, false)
	legacyState := legacy.poolSnapshot(time.Now().UnixNano())
	qualityState := quality.poolSnapshot(time.Now().UnixNano())
	if legacyState.Eligible != 1 || qualityState.Eligible != 0 {
		t.Fatalf("feedback crossed pool boundary: legacy eligible=%d quality eligible=%d", legacyState.Eligible, qualityState.Eligible)
	}
	foreign := testMulti(m.pools...)
	foreign.MarkSelectionResult(sel, true)
	if quality.poolSnapshot(time.Now().UnixNano()).Eligible != 0 {
		t.Fatal("feedback from a different supplier cleared the original pool cooldown")
	}
}

func TestWebshareMultiLegacyFeedbackAndSafePoolSnapshots(t *testing.T) {
	pool := &webshareDirectSupplier{}
	entry := testEntry("192.0.2.8", 8080, "proxy")
	installEntries(pool, entry)
	m := testMulti(&webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "pool-a", AccountID: "account-a", Class: PoolClassDC}, supplier: pool})
	m.MarkResult(entry.addr, false)
	if got := pool.poolSnapshot(time.Now().UnixNano()).Eligible; got != 0 {
		t.Fatalf("address feedback eligible count = %d, want cooldown", got)
	}
	if got := WebsharePools(m); len(got) != 1 || got[0].PoolID != "pool-a" || got[0].Class != PoolClassDC {
		t.Fatalf("safe pool info = %+v", got)
	}
	if got := WebsharePoolsSnapshot(m); len(got) != 1 || got[0].Eligible != 0 {
		t.Fatalf("pool state snapshot = %+v", got)
	}
	if got := WebsharePoolsSnapshot(noneSupplier{}); got != nil {
		t.Fatalf("non-provider snapshot = %+v, want nil", got)
	}
}

func TestHTTPClientForSelectionPinsProxyAndHonorsOptions(t *testing.T) {
	var dialCalls atomic.Int64
	var targetSeen atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() == "http://target.example/path" {
			targetSeen.Store(true)
		}
		w.Header().Set("Location", "http://other.example/redirect")
		w.WriteHeader(http.StatusFound)
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := &webshareDirectSupplier{}
	entry := &webshareDirectEntry{addr: proxyURL.Host, proxyURL: proxy.URL}
	named := &webshareNamedPool{WebsharePoolConfig: WebsharePoolConfig{ID: "dc", AccountID: "acct", Class: PoolClassDC}, supplier: pool}
	m := testMulti(named)
	selection := WebshareSelection{PoolID: "dc", AccountID: "acct", Class: PoolClassDC, owner: m, pool: named, entry: entry, proxyURL: entry.proxyURL}
	client := HTTPClientForSelection(selection, time.Second,
		WithDialContext(func(ctx context.Context, network, address string) (net.Conn, error) {
			dialCalls.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}),
		WithCheckRedirect(func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }),
	)
	resp, err := client.Get("http://target.example/path")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !targetSeen.Load() || dialCalls.Load() == 0 {
		t.Fatalf("pinned proxy result: status=%d targetSeen=%v dialCalls=%d", resp.StatusCode, targetSeen.Load(), dialCalls.Load())
	}
	invalid := HTTPClientForSelection(WebshareSelection{}, time.Second)
	resp2, err := invalid.Get("http://127.0.0.1:1/direct-forbidden")
	if resp2 != nil {
		resp2.Body.Close()
	}
	if !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("invalid selection error = %v, want ErrProxyUnavailable", err)
	}
}

func TestWebshareMultiRefreshFailureAndErrorsAreRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private response body", http.StatusForbidden)
	}))
	defer server.Close()
	withWebshareDirectBaseURL(t, server.URL)
	s := directSupplierForTest("api-secret", "2002")
	if err := s.refreshOnce(); err == nil {
		t.Fatal("refreshOnce succeeded on forbidden response")
	} else if strings.Contains(err.Error(), "api-secret") || strings.Contains(err.Error(), "2002") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("refresh error leaked configuration: %v", err)
	}
	state := s.poolSnapshot(time.Now().UnixNano())
	if state.ConsecutiveRefreshFailures != 1 || state.Total != 0 {
		t.Fatalf("unexpected failure state: %+v", state)
	}
	if got := (&webshareAPIError{Status: 401}).Error(); strings.Contains(got, "2002") {
		t.Fatalf("API error leaks URL: %s", got)
	}
}

func TestWebshareMultiRejectsHostilePaginationWithoutLeakingPlanID(t *testing.T) {
	var externalHits atomic.Int64
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { externalHits.Add(1) }))
	defer external.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(webshareListResponse{Next: external.URL + "/steal?plan_id=2002"})
	}))
	defer server.Close()
	withWebshareDirectBaseURL(t, server.URL)
	s := directSupplierForTest("api-secret", "2002")
	err := s.refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pagination") {
		t.Fatalf("refresh error = %v, want safe pagination rejection", err)
	}
	if externalHits.Load() != 0 {
		t.Fatalf("hostile pagination endpoint received %d requests", externalHits.Load())
	}
	if strings.Contains(err.Error(), "2002") || strings.Contains(err.Error(), "api-secret") || strings.Contains(err.Error(), external.URL) {
		t.Fatalf("pagination error leaked configuration: %v", err)
	}
}

func TestWebshareMultiRejectsPaginationCycles(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(webshareListResponse{Next: "?mode=direct&page=1&page_size=1000&plan_id=8123"})
	}))
	defer server.Close()
	withWebshareDirectBaseURL(t, server.URL)
	s := directSupplierForTest("api-secret", "8123")
	err := s.refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("refresh error = %v, want pagination cycle rejection", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("catalog loop made %d requests, want one", requests.Load())
	}
}

func TestWebsharePlanIDMustBePositiveDecimal(t *testing.T) {
	for _, planID := range []string{"1", "2002", "18446744073709551615"} {
		if !validWebsharePlanID(planID) {
			t.Errorf("validWebsharePlanID(%q) = false", planID)
		}
	}
	for _, planID := range []string{"", "0", "-1", "1.0", " 12", "12 ", "12/other", "18446744073709551616"} {
		if validWebsharePlanID(planID) {
			t.Errorf("validWebsharePlanID(%q) = true", planID)
		}
	}
}

func TestLoadWebsharePoolInfoValidatesStrictSafeConfig(t *testing.T) {
	dir := t.TempDir()
	path := writePoolConfig(t,
		poolConfig(t, dir, "legacy", "account-a", PoolClassDC, "1001", 0),
		poolConfig(t, dir, "isp", "account-a", PoolClassResidential, "3003", 0),
	)
	info, err := LoadWebsharePoolInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(info) != 2 || info[0].PoolID != "isp" || info[1].PoolID != "legacy" {
		t.Fatalf("safe pool labels = %+v", info)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "key-") {
		t.Fatal("pool config serialized API key contents")
	}
	if _, err := readWebsharePoolsConfig(path + ".missing"); err == nil || strings.Contains(err.Error(), path) {
		t.Fatalf("missing config error should be generic: %v", err)
	}
}

func TestReadWebsharePoolsConfigRejectsUnsafeShapes(t *testing.T) {
	dir := t.TempDir()
	valid := func(id, account, class, plan, keyPath string) map[string]any {
		return map[string]any{"id": id, "account_id": account, "class": class, "plan_id": plan, "api_key_file": keyPath}
	}
	keyPath := filepath.Join(dir, "key")
	cases := map[string]any{
		"empty pools":            map[string]any{"pools": []any{}},
		"unknown field":          map[string]any{"pools": []any{map[string]any{"id": "dc", "account_id": "acct", "class": "dc", "plan_id": "1001", "api_key_file": keyPath, "api_key": "inline-secret"}}},
		"invalid class":          map[string]any{"pools": []any{valid("dc", "acct", "rotating", "1001", keyPath)}},
		"invalid plan id":        map[string]any{"pools": []any{valid("dc", "acct", "dc", "1/2", keyPath)}},
		"relative key path":      map[string]any{"pools": []any{valid("dc", "acct", "dc", "1001", "relative.key")}},
		"negative weight":        map[string]any{"pools": []any{map[string]any{"id": "dc", "account_id": "acct", "class": "dc", "plan_id": "1001", "api_key_file": keyPath, "weight": -1}}},
		"duplicate pool id":      map[string]any{"pools": []any{valid("dc", "acct-a", "dc", "1001", keyPath), valid("dc", "acct-b", "dc", "1002", keyPath)}},
		"duplicate account plan": map[string]any{"pools": []any{valid("dc-a", "acct", "dc", "1001", keyPath), valid("dc-b", "acct", "residential", "1001", keyPath)}},
	}
	tooMany := make([]any, webshareMultiMaxPools+1)
	for i := range tooMany {
		tooMany[i] = valid(fmt.Sprintf("pool-%d", i), fmt.Sprintf("acct-%d", i), "dc", strconv.Itoa(i+1), keyPath)
	}
	cases["too many pools"] = map[string]any{"pools": tooMany}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "pools.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readWebsharePoolsConfig(path); err == nil || strings.Contains(err.Error(), "inline-secret") {
				t.Fatalf("unsafe config accepted or secret leaked: %v", err)
			}
		})
	}
	tooLarge := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(tooLarge, []byte(strings.Repeat(" ", webshareMultiConfigLimit+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWebsharePoolsConfig(tooLarge); err == nil {
		t.Fatal("accepted oversized configuration")
	}
	trailing := filepath.Join(t.TempDir(), "trailing.json")
	if err := os.WriteFile(trailing, []byte(`{"pools":[]} {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWebsharePoolsConfig(trailing); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	missing := NewFromConfig(Config{Supplier: "webshare_multi", WebsharePoolsFile: filepath.Join(dir, "missing.json")})
	if missing.Name() != "webshare_multi" || missing.ProxyURL() != "" {
		t.Fatalf("missing config should return an unavailable fail-closed supplier: %T %q", missing, missing.ProxyURL())
	}
}

func TestWebshareMultiCloseClearsOnlyItsMetricsPointer(t *testing.T) {
	var nilSupplier *webshareMultiSupplier
	nilSupplier.Close()
	first := testMulti()
	activeWebshareMultiSupplier.Store(first)
	first.Close()
	if got := activeWebshareMultiSupplier.Load(); got != nil {
		t.Fatal("Close left the active Webshare metrics pointer set")
	}

	first = testMulti()
	second := testMulti()
	activeWebshareMultiSupplier.Store(second)
	first.Close()
	if got := activeWebshareMultiSupplier.Load(); got != second {
		t.Fatal("closing an inactive supplier cleared another supplier's metrics pointer")
	}
	second.Close()
}

func TestWebshareCatalogPaginationRequiresSameOriginPathAndPlan(t *testing.T) {
	base := webshareDirectDefaultBaseURL
	current := base + "?mode=direct&page=1&page_size=1000&plan_id=1001"
	valid, err := safeWebshareNextURL(base, current, "?mode=direct&page=2&page_size=1000&plan_id=1001", "1001")
	if err != nil || !strings.HasSuffix(valid, "page=2&page_size=1000&plan_id=1001") {
		t.Fatalf("valid relative pagination URL = %q, %v", valid, err)
	}
	for _, next := range []string{
		"https://attacker.example/steal?plan_id=1001",
		"/different/path?plan_id=1001",
		"?page=2&plan_id=2002",
		"http://proxy.webshare.io/api/v2/proxy/list/?plan_id=1001",
	} {
		if _, err := safeWebshareNextURL(base, current, next, "1001"); err == nil {
			t.Errorf("accepted unsafe pagination URL %q", next)
		}
	}
}
