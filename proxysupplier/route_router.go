package proxysupplier

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	RouteProviderWebshare = "webshare_multi"
	RouteProviderStatic   = "static_proxy"
	EnvRouteRouterFile    = "PROXY_ROUTES_FILE"
	maxRouteConfigBytes   = 1 << 20
	maxRouteProviders     = 32
	maxRouteProfiles      = 64
	maxRouteCandidates    = 32
)

// RouteRouterConfig is the strict, provider-neutral request-routing shape.
// Webshare adapters reference their existing pool file; static endpoints refer
// to a protected file containing the credentialed proxy URL.
type RouteRouterConfig struct {
	Providers []RouteProviderConfig `json:"providers"`
	Profiles  []RouteProfileConfig  `json:"profiles,omitempty"`
}

// RouteProviderConfig configures one uniquely named provider adapter.
type RouteProviderConfig struct {
	ID        string                  `json:"id"`
	Type      string                  `json:"type"`
	PoolsFile string                  `json:"pools_file,omitempty"`
	Pools     []StaticRoutePoolConfig `json:"pools,omitempty"`
}

// StaticRoutePoolConfig describes one fixed proxy endpoint without exposing
// its credential-bearing URL in the route catalog.
type StaticRoutePoolConfig struct {
	ID           string    `json:"id"`
	AccountID    string    `json:"account_id"`
	Class        PoolClass `json:"class"`
	ProxyURLFile string    `json:"proxy_url_file"`
	Weight       int       `json:"weight,omitempty"`
}

// RouteProfileConfig is an explicitly requested ordered route chain.
type RouteProfileConfig struct {
	ID         string              `json:"id"`
	Candidates []RouteCandidateRef `json:"candidates"`
}

// RouteCandidateRef names one exact provider pool.
type RouteCandidateRef struct {
	ProviderID string `json:"provider_id"`
	PoolID     string `json:"pool_id"`
}

// LoadRouteRouterConfig reads the strict provider/profile catalog from path.
func LoadRouteRouterConfig(path string) (RouteRouterConfig, error) {
	var cfg RouteRouterConfig
	f, err := os.Open(path)
	if err != nil {
		return cfg, errors.New("proxysupplier: route configuration unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxRouteConfigBytes {
		return cfg, errors.New("proxysupplier: invalid route configuration")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRouteConfigBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxRouteConfigBytes {
		return RouteRouterConfig{}, errors.New("proxysupplier: invalid route configuration")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return RouteRouterConfig{}, errors.New("proxysupplier: invalid route configuration")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return RouteRouterConfig{}, errors.New("proxysupplier: invalid route configuration")
	}
	if err := validateRouteRouterConfig(cfg); err != nil {
		return RouteRouterConfig{}, err
	}
	return cfg, nil
}

// NewRouteRouterFromFile loads and starts all adapters from a strict config.
func NewRouteRouterFromFile(path string) (*RouteRouter, error) {
	cfg, err := LoadRouteRouterConfig(path)
	if err != nil {
		return nil, err
	}
	return NewRouteRouter(cfg)
}

// NewRouteRouterFromEnv enables explicit request routing only when
// PROXY_ROUTES_FILE is set. An unset variable returns (nil, nil), preserving
// existing service defaults.
func NewRouteRouterFromEnv() (*RouteRouter, error) {
	path := strings.TrimSpace(os.Getenv(EnvRouteRouterFile))
	if path == "" {
		return nil, nil
	}
	return NewRouteRouterFromFile(path)
}

// RouteRouter resolves explicit request intents. It is additive and does not
// replace or alter the process-level Supplier default.
type RouteRouter struct {
	mu        sync.Mutex
	providers map[string]routeProvider
	profiles  map[string][]RouteCandidate
	current   map[string]int
	closed    bool
}

type routeProvider interface {
	descriptors() []RouteCandidate
	weight(candidate RouteCandidate) int
	selectRoute(candidate RouteCandidate) (string, any, error)
	markRouteResult(token any, outcome RouteOutcome)
	close()
}

// NewRouteRouter validates the complete config before opening provider
// secrets, then constructs only its explicitly configured adapters.
func NewRouteRouter(cfg RouteRouterConfig) (*RouteRouter, error) {
	if err := validateRouteRouterConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateConfiguredRouteProfiles(cfg); err != nil {
		return nil, err
	}
	r := &RouteRouter{
		providers: make(map[string]routeProvider, len(cfg.Providers)),
		profiles:  make(map[string][]RouteCandidate, len(cfg.Profiles)),
		current:   make(map[string]int),
	}
	cleanup := func(err error) (*RouteRouter, error) {
		r.Close()
		return nil, err
	}
	for _, pc := range cfg.Providers {
		var provider routeProvider
		switch pc.Type {
		case RouteProviderWebshare:
			supplier := newWebshareMultiSupplier(pc.PoolsFile, parseNoProxy(""))
			selector, ok := supplier.(WebsharePoolSelector)
			if !ok {
				return cleanup(errors.New("proxysupplier: configured Webshare route provider unavailable"))
			}
			provider = &webshareRouteProvider{providerID: pc.ID, selector: selector, supplier: supplier}
		case RouteProviderStatic:
			sp, err := newStaticRouteProvider(pc.Pools)
			if err != nil {
				return cleanup(err)
			}
			sp.providerID = pc.ID
			provider = sp
		default:
			return cleanup(errors.New("proxysupplier: invalid route provider type"))
		}
		r.providers[pc.ID] = provider
	}
	for _, profile := range cfg.Profiles {
		chain := make([]RouteCandidate, 0, len(profile.Candidates))
		for _, ref := range profile.Candidates {
			candidate, ok := r.findCandidate(ref.ProviderID, ref.PoolID)
			if !ok {
				return cleanup(errors.New("proxysupplier: route profile references an unknown candidate"))
			}
			candidate.ProfileID = profile.ID
			chain = append(chain, candidate)
		}
		r.profiles[profile.ID] = chain
	}
	return r, nil
}

// validateConfiguredRouteProfiles resolves external Webshare labels before
// opening any provider key files. Static pool labels are already in cfg.
func validateConfiguredRouteProfiles(cfg RouteRouterConfig) error {
	providers := make(map[string]RouteProviderConfig, len(cfg.Providers))
	websharePools := make(map[string]map[string]struct{})
	for _, provider := range cfg.Providers {
		providers[provider.ID] = provider
		if provider.Type != RouteProviderWebshare {
			continue
		}
		info, err := LoadWebsharePoolInfo(provider.PoolsFile)
		if err != nil {
			return errors.New("proxysupplier: Webshare route provider configuration unavailable")
		}
		pools := make(map[string]struct{}, len(info))
		for _, pool := range info {
			pools[pool.PoolID] = struct{}{}
		}
		websharePools[provider.ID] = pools
	}
	for _, profile := range cfg.Profiles {
		for _, candidate := range profile.Candidates {
			provider := providers[candidate.ProviderID]
			if provider.Type == RouteProviderWebshare {
				if _, ok := websharePools[candidate.ProviderID][candidate.PoolID]; !ok {
					return errors.New("proxysupplier: route profile references an unknown candidate")
				}
			}
		}
	}
	return nil
}

// Close stops adapter-owned resources. It is safe to call more than once.
func (r *RouteRouter) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	providers := make([]routeProvider, 0, len(r.providers))
	for _, provider := range r.providers {
		providers = append(providers, provider)
	}
	r.mu.Unlock()
	for _, provider := range providers {
		provider.close()
	}
}

// ResolveCandidates returns safe descriptors in policy order. A pool ID is
// global only when unique; ambiguous IDs fail closed. Provider/class matches
// are returned in smooth weighted round-robin order to spread the first
// attempt fairly while preserving the remaining candidates for consumer-owned
// fallback.
func (r *RouteRouter) ResolveCandidates(intent RouteIntent) ([]RouteCandidate, error) {
	if r == nil || r.isClosed() || countRouteSelectors(intent.PoolID != "", intent.ProviderID != "", intent.ProfileID != "") != 1 {
		return nil, errors.New("proxysupplier: invalid route intent")
	}
	for _, id := range []string{intent.PoolID, intent.ProviderID, intent.ProfileID} {
		if id != "" && !webshareSafeID.MatchString(id) {
			return nil, errors.New("proxysupplier: invalid route intent")
		}
	}
	if intent.Class != "" && intent.Class != PoolClassDC && intent.Class != PoolClassResidential {
		return nil, errors.New("proxysupplier: invalid route class")
	}
	if intent.ProfileID != "" {
		if intent.Class != "" {
			return nil, errors.New("proxysupplier: conflicting route selectors")
		}
		chain, ok := r.profiles[intent.ProfileID]
		if !ok {
			return nil, errors.New("proxysupplier: route profile unavailable")
		}
		return append([]RouteCandidate(nil), chain...), nil
	}
	if intent.PoolID != "" {
		if intent.Class != "" {
			return nil, errors.New("proxysupplier: conflicting route selectors")
		}
		var found []RouteCandidate
		for _, provider := range r.providers {
			for _, candidate := range provider.descriptors() {
				if candidate.PoolID == intent.PoolID {
					found = append(found, candidate)
				}
			}
		}
		if len(found) != 1 {
			return nil, errors.New("proxysupplier: route pool unavailable or ambiguous")
		}
		return found, nil
	}
	provider, ok := r.providers[intent.ProviderID]
	if !ok {
		return nil, errors.New("proxysupplier: route provider unavailable")
	}
	candidates := provider.descriptors()
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if intent.Class == "" || candidate.Class == intent.Class {
			filtered = append(filtered, candidate)
		}
	}
	if len(filtered) == 0 {
		return nil, errors.New("proxysupplier: route class unavailable")
	}
	return r.weightedOrder(intent.ProviderID+"\x00"+string(intent.Class), provider, filtered), nil
}

// SelectRoute selects exactly candidate. It never substitutes another pool or
// provider. Residential selection is denied unless the caller confirms that an
// external policy permit has already been acquired.
func (r *RouteRouter) SelectRoute(candidate RouteCandidate, allowResidential bool) (RouteSelection, error) {
	if r == nil || r.isClosed() || (candidate.Class == PoolClassResidential && !allowResidential) {
		return RouteSelection{}, ErrProxyUnavailable
	}
	provider, ok := r.providers[candidate.ProviderID]
	if !ok {
		return RouteSelection{}, ErrProxyUnavailable
	}
	actual, found := r.findCandidate(candidate.ProviderID, candidate.PoolID)
	if !found || actual.AccountID != candidate.AccountID || actual.Class != candidate.Class {
		return RouteSelection{}, ErrProxyUnavailable
	}
	proxyURL, token, err := provider.selectRoute(actual)
	if err != nil {
		return RouteSelection{}, ErrProxyUnavailable
	}
	actual.ProfileID = candidate.ProfileID
	return RouteSelection{descriptor: actual, proxyURL: proxyURL, owner: r, provider: provider, token: token}, nil
}

// MarkRouteResult records feedback only against the exact selection issued by
// this router. Selections from another router or a fabricated value are ignored.
func (r *RouteRouter) MarkRouteResult(selection RouteSelection, outcome RouteOutcome) {
	if r == nil || selection.owner != r || selection.provider == nil || selection.token == nil ||
		outcome < RouteOutcomeSuccess || outcome > RouteOutcomeCanceled {
		return
	}
	selection.provider.markRouteResult(selection.token, outcome)
}

func (r *RouteRouter) findCandidate(providerID, poolID string) (RouteCandidate, bool) {
	provider, ok := r.providers[providerID]
	if !ok {
		return RouteCandidate{}, false
	}
	for _, candidate := range provider.descriptors() {
		if candidate.PoolID == poolID {
			return candidate, true
		}
	}
	return RouteCandidate{}, false
}

func (r *RouteRouter) weightedOrder(key string, provider routeProvider, candidates []RouteCandidate) []RouteCandidate {
	r.mu.Lock()
	defer r.mu.Unlock()
	copyCandidates := append([]RouteCandidate(nil), candidates...)
	current := make([]int, len(copyCandidates))
	total := 0
	for i, candidate := range copyCandidates {
		weight := provider.weight(candidate)
		if weight < 1 {
			weight = 1
		}
		r.current[key+"\x00"+candidate.PoolID] += weight
		current[i] = r.current[key+"\x00"+candidate.PoolID]
		total += weight
	}
	start := 0
	for i := range copyCandidates {
		if current[i] > current[start] {
			start = i
		}
	}
	r.current[key+"\x00"+copyCandidates[start].PoolID] -= total
	out := make([]RouteCandidate, 0, len(copyCandidates))
	out = append(out, copyCandidates[start])
	for step := 1; step < len(copyCandidates); step++ {
		out = append(out, copyCandidates[(start+step)%len(copyCandidates)])
	}
	return out
}

func (r *RouteRouter) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func validateRouteRouterConfig(cfg RouteRouterConfig) error {
	if len(cfg.Providers) == 0 || len(cfg.Providers) > maxRouteProviders || len(cfg.Profiles) > maxRouteProfiles {
		return errors.New("proxysupplier: invalid route configuration")
	}
	providers := make(map[string]RouteProviderConfig, len(cfg.Providers))
	ids := make(map[string]map[string]struct{}, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if !webshareSafeID.MatchString(p.ID) {
			return errors.New("proxysupplier: invalid route configuration")
		}
		if _, exists := providers[p.ID]; exists {
			return errors.New("proxysupplier: invalid route configuration")
		}
		providers[p.ID] = p
		ids[p.ID] = map[string]struct{}{}
		switch p.Type {
		case RouteProviderWebshare:
			if !filepathIsAbsolute(p.PoolsFile) || len(p.Pools) != 0 {
				return errors.New("proxysupplier: invalid route configuration")
			}
		case RouteProviderStatic:
			if p.PoolsFile != "" || len(p.Pools) == 0 || len(p.Pools) > maxRouteCandidates {
				return errors.New("proxysupplier: invalid route configuration")
			}
			for _, pool := range p.Pools {
				if !webshareSafeID.MatchString(pool.ID) || !webshareSafeID.MatchString(pool.AccountID) ||
					(pool.Class != PoolClassDC && pool.Class != PoolClassResidential) ||
					!filepathIsAbsolute(pool.ProxyURLFile) || pool.Weight < 0 || pool.Weight > webshareMultiMaxWeight {
					return errors.New("proxysupplier: invalid route configuration")
				}
				if _, exists := ids[p.ID][pool.ID]; exists {
					return errors.New("proxysupplier: invalid route configuration")
				}
				ids[p.ID][pool.ID] = struct{}{}
			}
		default:
			return errors.New("proxysupplier: invalid route configuration")
		}
	}
	seenProfiles := make(map[string]struct{}, len(cfg.Profiles))
	for _, profile := range cfg.Profiles {
		if !webshareSafeID.MatchString(profile.ID) || len(profile.Candidates) == 0 || len(profile.Candidates) > maxRouteCandidates {
			return errors.New("proxysupplier: invalid route configuration")
		}
		if _, exists := seenProfiles[profile.ID]; exists {
			return errors.New("proxysupplier: invalid route configuration")
		}
		seenProfiles[profile.ID] = struct{}{}
		seen := map[string]struct{}{}
		for _, candidate := range profile.Candidates {
			key := candidate.ProviderID + "\x00" + candidate.PoolID
			if !webshareSafeID.MatchString(candidate.ProviderID) || !webshareSafeID.MatchString(candidate.PoolID) {
				return errors.New("proxysupplier: invalid route configuration")
			}
			if _, exists := seen[key]; exists {
				return errors.New("proxysupplier: invalid route configuration")
			}
			if _, exists := providers[candidate.ProviderID]; !exists {
				return errors.New("proxysupplier: invalid route configuration")
			}
			if providers[candidate.ProviderID].Type == RouteProviderStatic {
				if _, exists := ids[candidate.ProviderID][candidate.PoolID]; !exists {
					return errors.New("proxysupplier: invalid route configuration")
				}
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

type webshareRouteProvider struct {
	providerID string
	selector   WebsharePoolSelector
	supplier   Supplier
}

func (p *webshareRouteProvider) descriptors() []RouteCandidate {
	infos := p.selector.Pools()
	out := make([]RouteCandidate, 0, len(infos))
	for _, info := range infos {
		out = append(out, RouteCandidate{ProviderID: p.providerID, PoolID: info.PoolID, AccountID: info.AccountID, Class: info.Class})
	}
	return out
}
func (p *webshareRouteProvider) weight(c RouteCandidate) int {
	for _, pool := range p.selector.Pools() {
		if pool.PoolID == c.PoolID {
			if pool.Eligible > 0 {
				return pool.Eligible
			}
		}
	}
	return 1
}
func (p *webshareRouteProvider) selectRoute(c RouteCandidate) (string, any, error) {
	selection, err := p.selector.SelectPoolID(c.PoolID)
	if err != nil || selection.Class != c.Class || selection.AccountID != c.AccountID {
		return "", nil, ErrProxyUnavailable
	}
	u, err := selection.ProxyURL()
	if err != nil {
		return "", nil, ErrProxyUnavailable
	}
	return u.String(), selection, nil
}
func (p *webshareRouteProvider) markRouteResult(token any, outcome RouteOutcome) {
	selection, ok := token.(WebshareSelection)
	if !ok {
		return
	}
	reporter, ok := p.supplier.(WebshareSelectionReporter)
	if !ok {
		return
	}
	success := outcome == RouteOutcomeSuccess || outcome == RouteOutcomeOriginResponse || outcome == RouteOutcomeCanceled
	reporter.MarkSelectionResult(selection, success)
}
func (p *webshareRouteProvider) close() {
	if closer, ok := p.supplier.(interface{ Close() }); ok {
		closer.Close()
	}
}

type staticRouteProvider struct {
	providerID string
	pools      []staticRoutePool
}
type staticRoutePool struct {
	candidate RouteCandidate
	proxyURL  string
	weight    int
}

func newStaticRouteProvider(cfg []StaticRoutePoolConfig) (*staticRouteProvider, error) {
	p := &staticRouteProvider{pools: make([]staticRoutePool, 0, len(cfg))}
	for _, pool := range cfg {
		b, err := readSecretFile(pool.ProxyURLFile, 16<<10)
		if err != nil {
			return nil, errors.New("proxysupplier: static route provider unavailable")
		}
		raw := strings.TrimSpace(string(b))
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" || u.Path != "" || u.RawQuery != "" {
			return nil, errors.New("proxysupplier: invalid static route endpoint")
		}
		if _, portErr := strconvPort(u); portErr != nil {
			return nil, errors.New("proxysupplier: invalid static route endpoint")
		}
		weight := pool.Weight
		if weight == 0 {
			weight = 1
		}
		p.pools = append(p.pools, staticRoutePool{candidate: RouteCandidate{PoolID: pool.ID, AccountID: pool.AccountID, Class: pool.Class}, proxyURL: raw, weight: weight})
	}
	sort.Slice(p.pools, func(i, j int) bool { return p.pools[i].candidate.PoolID < p.pools[j].candidate.PoolID })
	return p, nil
}

func (p *staticRouteProvider) descriptors() []RouteCandidate {
	out := make([]RouteCandidate, 0, len(p.pools))
	for _, pool := range p.pools {
		candidate := pool.candidate
		candidate.ProviderID = p.providerID
		out = append(out, candidate)
	}
	return out
}
func (p *staticRouteProvider) weight(c RouteCandidate) int {
	for _, pool := range p.pools {
		if pool.candidate.PoolID == c.PoolID {
			return pool.weight
		}
	}
	return 1
}
func (p *staticRouteProvider) selectRoute(c RouteCandidate) (string, any, error) {
	for _, pool := range p.pools {
		if pool.candidate.PoolID == c.PoolID && pool.candidate.Class == c.Class && pool.candidate.AccountID == c.AccountID {
			candidate := pool.candidate
			candidate.ProviderID = p.providerID
			return pool.proxyURL, candidate, nil
		}
	}
	return "", nil, ErrProxyUnavailable
}
func (*staticRouteProvider) markRouteResult(any, RouteOutcome) {}
func (*staticRouteProvider) close()                            {}

func readSecretFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("invalid secret file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(b) == 0 || int64(len(b)) > limit {
		return nil, errors.New("invalid secret file")
	}
	return b, nil
}

func strconvPort(u *url.URL) (string, error) {
	if u.Hostname() == "" {
		return "", errors.New("missing host")
	}
	if u.Port() != "" {
		return u.Port(), nil
	}
	if u.Scheme == "http" {
		return "80", nil
	}
	return "443", nil
}

var _ RouteSelector = (*RouteRouter)(nil)
