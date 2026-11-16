package proxysupplier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/go-common/graph"
)

const (
	webshareMultiConfigLimit   = 1 << 20
	webshareMultiMaxPools      = 32
	webshareMultiMaxWeight     = 1_000_000
	webshareMultiMaxKeyBytes   = 16 << 10
	webshareDirectMaxPages     = 1_000
	webshareDirectMaxEntries   = 100_000
	webshareDirectMaxPageBytes = 4 << 20
)

var webshareSafeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// WebsharePoolsConfig is the strict top-level shape of WEBSHARE_POOLS_FILE.
type WebsharePoolsConfig struct {
	Pools []WebsharePoolConfig `json:"pools"`
}

// WebsharePoolConfig defines one account-plan catalog. API keys are read from
// ApiKeyFile; they are never accepted inline. Weight zero means use the live
// eligible catalog size as its share of the default DC traffic.
type WebsharePoolConfig struct {
	ID         string    `json:"id"`
	AccountID  string    `json:"account_id"`
	Class      PoolClass `json:"class"`
	PlanID     string    `json:"plan_id"`
	APIKeyFile string    `json:"api_key_file"`
	Weight     int       `json:"weight,omitempty"`
}

type webshareNamedPool struct {
	WebsharePoolConfig
	supplier *webshareDirectSupplier
}

// webshareMultiSupplier combines separately refreshed account-plan catalogs.
// Its default Supplier API is deliberately DC-only. Policy callers use the
// typed selector to explicitly reach a particular class or named pool.
type webshareMultiSupplier struct {
	pools         []*webshareNamedPool
	rules         *noProxyRules
	defaultPoolID string
	mu            sync.Mutex
	current       map[string]int
}

var activeWebshareMultiSupplier atomic.Pointer[webshareMultiSupplier]

type unavailableWebshareMultiSupplier struct{ rules *noProxyRules }

func (*unavailableWebshareMultiSupplier) Name() string     { return "webshare_multi" }
func (*unavailableWebshareMultiSupplier) ProxyURL() string { return "" }
func (s *unavailableWebshareMultiSupplier) Bypass(host string) bool {
	return s.rules.Match(host)
}

func (s *webshareMultiSupplier) Name() string { return "webshare_multi" }
func (s *webshareMultiSupplier) Bypass(host string) bool {
	return s.rules.Match(host)
}

// Close stops every per-plan catalog refresh goroutine and releases the
// process-wide metrics pointer if it still refers to this supplier.
func (s *webshareMultiSupplier) Close() {
	if s == nil {
		return
	}
	closeWebsharePools(s.pools)
	activeWebshareMultiSupplier.CompareAndSwap(s, nil)
}

// ProxyURL preserves Supplier compatibility and selects only from DC pools.
// A missing/unhealthy DC pool returns empty; HTTPClient treats this supplier
// as fail-closed and never silently sends the request directly.
func (s *webshareMultiSupplier) ProxyURL() string {
	sel, err := s.SelectDefaultPool()
	if err != nil {
		return ""
	}
	return sel.proxyURL
}

// SelectDefaultPool follows the process-level default route. Unless explicitly
// pinned to a named DC pool, it uses the capacity-weighted DC selector.
func (s *webshareMultiSupplier) SelectDefaultPool() (WebshareSelection, error) {
	if s.defaultPoolID != "" {
		return s.SelectPoolID(s.defaultPoolID)
	}
	return s.SelectPool(PoolClassDC)
}

// SelectPool picks one eligible member of the requested class. Excluded pool
// IDs are skipped, allowing a caller to stage bounded retries without
// accidentally reusing the same account plan. A residential selection is
// never made implicitly by the default ProxyURL path.
func (s *webshareMultiSupplier) SelectPool(class PoolClass, excludePoolIDs ...string) (WebshareSelection, error) {
	if class != PoolClassDC && class != PoolClassResidential {
		return WebshareSelection{}, ErrProxyUnavailable
	}
	excluded := make(map[string]struct{}, len(excludePoolIDs))
	for _, id := range excludePoolIDs {
		excluded[id] = struct{}{}
	}
	eligible := make([]*webshareNamedPool, 0, len(s.pools))
	weights := make([]int, 0, len(s.pools))
	totalWeight := 0
	for _, p := range s.pools {
		if p.Class != class {
			continue
		}
		if _, skip := excluded[p.ID]; skip {
			continue
		}
		count := p.supplier.eligibleCount()
		if count == 0 {
			continue
		}
		weight := p.Weight
		if weight <= 0 {
			weight = count
		}
		eligible = append(eligible, p)
		weights = append(weights, weight)
		totalWeight += weight
	}
	if totalWeight == 0 {
		return WebshareSelection{}, ErrProxyUnavailable
	}
	// Smooth weighted round-robin avoids long runs from one account while
	// keeping the long-run share proportional to available IP capacity.
	s.mu.Lock()
	if s.current == nil {
		s.current = make(map[string]int, len(s.pools))
	}
	chosen := 0
	for i, p := range eligible {
		s.current[p.ID] += weights[i]
		if s.current[p.ID] > s.current[eligible[chosen].ID] {
			chosen = i
		}
	}
	s.current[eligible[chosen].ID] -= totalWeight
	s.mu.Unlock()
	for step := 0; step < len(eligible); step++ {
		p := eligible[(chosen+step)%len(eligible)]
		if selection, ok := s.selectionFor(p); ok {
			return selection, nil
		}
	}
	return WebshareSelection{}, ErrProxyUnavailable
}

// SelectPoolID selects only the named pool, regardless of its class. The
// caller must make the policy decision to name a residential pool explicitly.
func (s *webshareMultiSupplier) SelectPoolID(poolID string) (WebshareSelection, error) {
	for _, p := range s.pools {
		if p.ID == poolID {
			if selection, ok := s.selectionFor(p); ok {
				return selection, nil
			}
			return WebshareSelection{}, ErrProxyUnavailable
		}
	}
	return WebshareSelection{}, ErrProxyUnavailable
}

func (s *webshareMultiSupplier) selectionFor(p *webshareNamedPool) (WebshareSelection, bool) {
	e := p.supplier.nextEntry()
	if e == nil {
		return WebshareSelection{}, false
	}
	return WebshareSelection{
		PoolID: p.ID, AccountID: p.AccountID, Class: p.Class,
		owner: s, pool: p, entry: e, proxyURL: e.proxyURL,
	}, true
}

// MarkSelectionResult updates only the exact pool entry that produced the
// selection. Selections from another supplier, or from a pool no longer in
// this supplier, are harmless no-ops.
func (s *webshareMultiSupplier) MarkSelectionResult(selection WebshareSelection, ok bool) {
	if selection.owner != s || selection.pool == nil || selection.entry == nil {
		return
	}
	for _, p := range s.pools {
		if p == selection.pool && p.ID == selection.PoolID {
			p.supplier.mu.RLock()
			found := false
			for _, e := range p.supplier.list {
				if e == selection.entry {
					found = true
					break
				}
			}
			if found {
				p.supplier.markEntryResult(selection.entry, ok)
			}
			p.supplier.mu.RUnlock()
			return
		}
	}
}

// MarkResult implements the older address-only feedback contract. Typed
// callers should use MarkSelectionResult to avoid ambiguity when accounts
// happen to expose the same address.
func (s *webshareMultiSupplier) MarkResult(addr string, ok bool) {
	for _, p := range s.pools {
		p.supplier.MarkResult(addr, ok)
	}
}

// Pools reports safe pool labels and a point-in-time health snapshot.
func (s *webshareMultiSupplier) Pools() []WebsharePoolInfo {
	result := make([]WebsharePoolInfo, 0, len(s.pools))
	for _, p := range s.pools {
		state := p.supplier.poolSnapshot(time.Now().UnixNano())
		result = append(result, WebsharePoolInfo{
			PoolID: p.ID, AccountID: p.AccountID, Class: p.Class, Default: p.ID == s.defaultPoolID,
			Total: state.Total, Eligible: state.Eligible, InCooldown: state.InCooldown,
		})
	}
	return result
}

// WebsharePoolStates implements WebsharePoolStateProvider.
func (s *webshareMultiSupplier) WebsharePoolStates() []WebsharePoolState {
	return websharePoolsSnapshot(s)
}

// WebsharePools returns safe configured pool identities/health for a supplier
// that supports typed Webshare selection. Other suppliers return nil.
func WebsharePools(s Supplier) []WebsharePoolInfo {
	if selector, ok := s.(WebsharePoolSelector); ok {
		return selector.Pools()
	}
	return nil
}

// LoadWebsharePoolInfo reads and validates only the non-secret configuration
// labels. It does not open any API-key files, so callers can validate intended
// pool IDs and classes before constructing the supplier.
func LoadWebsharePoolInfo(path string) ([]WebsharePoolInfo, error) {
	cfg, err := readWebsharePoolsConfig(path)
	if err != nil {
		return nil, err
	}
	result := make([]WebsharePoolInfo, 0, len(cfg.Pools))
	for _, p := range cfg.Pools {
		result = append(result, WebsharePoolInfo{PoolID: p.ID, AccountID: p.AccountID, Class: p.Class})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PoolID < result[j].PoolID })
	return result, nil
}

func (s *webshareDirectSupplier) eligibleCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UnixNano()
	count := 0
	for _, e := range s.list {
		if e.eligible(now) {
			count++
		}
	}
	return count
}

// WebsharePoolState is the safe, per-pool refresh/cooldown snapshot exposed by
// WebsharePoolsSnapshot and the pool metrics collector.
type WebsharePoolState struct {
	WebsharePoolInfo
	RefreshSuccesses           uint64
	RefreshFailures            uint64
	ConsecutiveRefreshFailures int
	LastRefreshAgeSeconds      float64
}

// WebsharePoolsSnapshot returns a per-pool snapshot for webshare_multi, or an
// empty slice for other supplier kinds.
func WebsharePoolsSnapshot(s Supplier) []WebsharePoolState {
	if s == nil {
		return websharePoolsSnapshot(activeWebshareMultiSupplier.Load())
	}
	provider, ok := s.(WebsharePoolStateProvider)
	if !ok {
		return nil
	}
	return provider.WebsharePoolStates()
}

func websharePoolsSnapshot(m *webshareMultiSupplier) []WebsharePoolState {
	if m == nil {
		return nil
	}
	now := time.Now().UnixNano()
	result := make([]WebsharePoolState, 0, len(m.pools))
	for _, p := range m.pools {
		state := p.supplier.poolSnapshot(now)
		result = append(result, WebsharePoolState{
			WebsharePoolInfo: WebsharePoolInfo{
				PoolID: p.ID, AccountID: p.AccountID, Class: p.Class, Default: p.ID == m.defaultPoolID,
				Total: state.Total, Eligible: state.Eligible, InCooldown: state.InCooldown,
			},
			RefreshSuccesses: state.RefreshSuccesses, RefreshFailures: state.RefreshFailures,
			ConsecutiveRefreshFailures: state.ConsecutiveRefreshFailures,
			LastRefreshAgeSeconds:      state.LastRefreshAgeSeconds,
		})
	}
	return result
}

func newWebshareMultiSupplier(path string, rules *noProxyRules, defaultPoolIDs ...string) Supplier {
	activeWebshareMultiSupplier.Store(nil)
	defaultPoolID := ""
	if len(defaultPoolIDs) > 0 {
		defaultPoolID = strings.TrimSpace(defaultPoolIDs[0])
	}
	if len(defaultPoolIDs) > 1 || (defaultPoolID != "" && !webshareSafeID.MatchString(defaultPoolID)) {
		return &unavailableWebshareMultiSupplier{rules: rules}
	}
	if strings.TrimSpace(path) == "" {
		return &unavailableWebshareMultiSupplier{rules: rules}
	}
	cfg, err := readWebsharePoolsConfig(path)
	if err != nil {
		return &unavailableWebshareMultiSupplier{rules: rules}
	}
	if defaultPoolID != "" {
		found := false
		for _, pool := range cfg.Pools {
			if pool.ID == defaultPoolID && pool.Class == PoolClassDC {
				found = true
				break
			}
		}
		if !found {
			return &unavailableWebshareMultiSupplier{rules: rules}
		}
	}
	seen := make(map[string]struct{}, len(cfg.Pools))
	pools := make([]*webshareNamedPool, 0, len(cfg.Pools))
	for _, pc := range cfg.Pools {
		if _, exists := seen[pc.ID]; exists {
			return &unavailableWebshareMultiSupplier{rules: rules}
		}
		seen[pc.ID] = struct{}{}
		keyFile, err := os.Open(pc.APIKeyFile)
		if err != nil {
			closeWebsharePools(pools)
			return &unavailableWebshareMultiSupplier{rules: rules}
		}
		keyInfo, statErr := keyFile.Stat()
		if statErr != nil || !keyInfo.Mode().IsRegular() || keyInfo.Size() <= 0 || keyInfo.Size() > webshareMultiMaxKeyBytes {
			_ = keyFile.Close()
			closeWebsharePools(pools)
			return &unavailableWebshareMultiSupplier{rules: rules}
		}
		key, err := io.ReadAll(io.LimitReader(keyFile, webshareMultiMaxKeyBytes+1))
		_ = keyFile.Close()
		if err != nil {
			closeWebsharePools(pools)
			return &unavailableWebshareMultiSupplier{rules: rules}
		}
		if len(key) == 0 || len(key) > webshareMultiMaxKeyBytes {
			closeWebsharePools(pools)
			return &unavailableWebshareMultiSupplier{rules: rules}
		}
		apiKey := strings.TrimSpace(string(key))
		invalidKey := apiKey == ""
		for _, r := range apiKey {
			if r < 0x21 || r > 0x7e {
				invalidKey = true
				break
			}
		}
		if invalidKey {
			closeWebsharePools(pools)
			return &unavailableWebshareMultiSupplier{rules: rules}
		}
		core := newWebshareDirectSupplierCore(Config{WebshareAPIKey: apiKey, WebsharePlanID: pc.PlanID, NoProxy: ""}, false)
		if core == nil {
			closeWebsharePools(pools)
			return &unavailableWebshareMultiSupplier{rules: rules}
		}
		pools = append(pools, &webshareNamedPool{WebsharePoolConfig: pc, supplier: core})
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].ID < pools[j].ID })
	m := &webshareMultiSupplier{pools: pools, rules: rules, defaultPoolID: defaultPoolID}
	activeWebshareMultiSupplier.Store(m)
	return m
}

func closeWebsharePools(pools []*webshareNamedPool) {
	for _, p := range pools {
		p.supplier.Close()
	}
}

func readWebsharePoolsConfig(path string) (WebsharePoolsConfig, error) {
	var cfg WebsharePoolsConfig
	f, err := os.Open(path)
	if err != nil {
		return cfg, errors.New("proxysupplier: Webshare pool configuration unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > webshareMultiConfigLimit {
		return cfg, errors.New("proxysupplier: invalid Webshare pool configuration")
	}
	dec := json.NewDecoder(io.LimitReader(f, webshareMultiConfigLimit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, errors.New("proxysupplier: invalid Webshare pool configuration")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return cfg, errors.New("proxysupplier: invalid Webshare pool configuration")
	}
	if len(cfg.Pools) == 0 || len(cfg.Pools) > webshareMultiMaxPools {
		return cfg, errors.New("proxysupplier: invalid Webshare pool configuration")
	}
	seen := make(map[string]struct{}, len(cfg.Pools))
	seenPlan := make(map[string]struct{}, len(cfg.Pools))
	for _, p := range cfg.Pools {
		if !webshareSafeID.MatchString(p.ID) || !webshareSafeID.MatchString(p.AccountID) ||
			(p.Class != PoolClassDC && p.Class != PoolClassResidential) ||
			!validWebsharePlanID(p.PlanID) ||
			!filepathIsAbsolute(p.APIKeyFile) || p.Weight < 0 || p.Weight > webshareMultiMaxWeight {
			return cfg, errors.New("proxysupplier: invalid Webshare pool configuration")
		}
		if _, exists := seen[p.ID]; exists {
			return cfg, errors.New("proxysupplier: invalid Webshare pool configuration")
		}
		planKey := p.AccountID + "\x00" + p.PlanID
		if _, exists := seenPlan[planKey]; exists {
			return cfg, errors.New("proxysupplier: invalid Webshare pool configuration")
		}
		seen[p.ID] = struct{}{}
		seenPlan[planKey] = struct{}{}
	}
	return cfg, nil
}

func validWebsharePlanID(planID string) bool {
	if len(planID) == 0 || len(planID) > 20 {
		return false
	}
	for _, r := range planID {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(planID, 10, 64)
	return err == nil && n > 0
}

func filepathIsAbsolute(path string) bool {
	return filepath.IsAbs(path) && path != "/"
}

func safeWebshareNextURL(baseURL, currentURL, next, planID string) (string, error) {
	if next == "" {
		return "", nil
	}
	base, err := url.Parse(baseURL)
	if err != nil || !allowedWebshareCatalogURL(base) {
		return "", errors.New("proxysupplier: invalid Webshare catalog endpoint")
	}
	current, err := url.Parse(currentURL)
	if err != nil || !safeWebshareURL(baseURL, currentURL, currentURL, planID) {
		return "", errors.New("proxysupplier: invalid Webshare pagination URL")
	}
	rel, err := url.Parse(next)
	if err != nil || rel.User != nil || rel.Fragment != "" {
		return "", errors.New("proxysupplier: invalid Webshare pagination URL")
	}
	resolved := current.ResolveReference(rel)
	if !safeWebshareURL(baseURL, currentURL, resolved.String(), planID) {
		return "", errors.New("proxysupplier: invalid Webshare pagination URL")
	}
	return resolved.String(), nil
}

func safeWebshareURL(baseURL, currentURL, candidateURL, planID string) bool {
	base, err := url.Parse(baseURL)
	if err != nil || !allowedWebshareCatalogURL(base) {
		return false
	}
	current, err := url.Parse(currentURL)
	if err != nil || !sameWebshareOrigin(base, current) || current.EscapedPath() != base.EscapedPath() || current.User != nil || current.Fragment != "" {
		return false
	}
	candidate, err := url.Parse(candidateURL)
	if err != nil || !sameWebshareOrigin(base, candidate) || candidate.EscapedPath() != base.EscapedPath() || candidate.User != nil || candidate.Fragment != "" {
		return false
	}
	if planID != "" && candidate.Query().Get("plan_id") != planID {
		return false
	}
	return true
}

func allowedWebshareCatalogURL(u *url.URL) bool {
	if u == nil || u.User != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	if strings.EqualFold(u.Scheme, "https") {
		return true
	}
	// HTTP is allowed only for package tests that use an in-process loopback
	// server. No remotely reachable insecure catalog endpoint is accepted.
	host, _, err := net.SplitHostPort(u.Host)
	return err == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback() && strings.EqualFold(u.Scheme, "http")
}

func sameWebshareOrigin(a, b *url.URL) bool {
	if !allowedWebshareCatalogURL(a) || !allowedWebshareCatalogURL(b) || !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return port(a) == port(b)
}

// HTTPClientOption adjusts the safe transport used by HTTPClientForSelection.
type HTTPClientOption func(*http.Client, *http.Transport)

// WithCheckRedirect overrides redirect handling for an explicitly selected
// proxy client. A caller with a destination allowlist should install a guard
// here so redirects cannot move metered traffic to an unrelated host.
func WithCheckRedirect(check func(*http.Request, []*http.Request) error) HTTPClientOption {
	return func(c *http.Client, _ *http.Transport) { c.CheckRedirect = check }
}

// WithDialContext supplies a custom dial function, e.g. a metered connection
// wrapper. The selected proxy remains fixed and unavailable selections still
// fail closed.
func WithDialContext(dial func(context.Context, string, string) (net.Conn, error)) HTTPClientOption {
	return func(_ *http.Client, t *http.Transport) {
		if dial != nil {
			t.DialContext = dial
		}
	}
}

// HTTPClientForSelection builds a graph-observed, fail-closed client pinned to
// one explicit Webshare selection. It never reselects or sends direct.
func HTTPClientForSelection(selection WebshareSelection, timeout time.Duration, opts ...HTTPClientOption) *http.Client {
	proxyURL, err := selection.ProxyURL()
	valid := err == nil
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	if valid {
		transport.Proxy = http.ProxyURL(proxyURL)
	} else {
		transport.Proxy = func(*http.Request) (*url.URL, error) { return nil, ErrProxyUnavailable }
	}
	client := &http.Client{
		Transport: &graphObservedTransport{Transport: transport, observed: graph.RoundTripper(transport)},
		Timeout:   timeout,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(client, transport)
		}
	}
	return client
}

var _ WebsharePoolSelector = (*webshareMultiSupplier)(nil)
var _ WebshareDefaultSelector = (*webshareMultiSupplier)(nil)
var _ WebshareSelectionReporter = (*webshareMultiSupplier)(nil)
