package proxysupplier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// webshareDirectDefaultBaseURL/PageSize/RefreshInterval/HTTPTimeout mirror
// the values go-html-proxy's webshareproxy package validated in production
// (2026-08-25/26) before this was promoted here as a shared primitive.
const (
	webshareDirectDefaultPageSize        = 100
	webshareDirectDefaultRefreshInterval = 10 * time.Minute
	webshareDirectDefaultHTTPTimeout     = 15 * time.Second
	// webshareDirectRefreshBudget bounds one full paginated refresh
	// regardless of pool size -- sized for tens of thousands of entries
	// (500 pages at page_size=100 is a 50k-entry account), not just
	// whatever the account holds today. A fixed "2x one page's timeout"
	// budget silently starts failing refreshes the moment the account's
	// pool outgrows a couple thousand entries.
	webshareDirectRefreshBudget = 5 * time.Minute

	// webshareDirectMaxConsecutiveFailures is how many bad results (per
	// MarkResult) a single exit IP tolerates before ProxyURL() starts
	// skipping it. Set to 1, not the 3 go-html-proxy's own webshareproxy
	// package uses for connection-level failures -- traced live
	// 2026-08-26 against a rate-limiting target (DuckDuckGo): a single
	// direct, unproxied request from a fresh source succeeded, and the
	// SAME source's very next request (seconds later) was already
	// throttled. Whatever signal MarkResult is fed here reflects an
	// application-level judgment (e.g. "got a bot-challenge page"), not a
	// noisy connection blip, so there's no reason to tolerate a repeat
	// before backing off.
	webshareDirectMaxConsecutiveFailures = 1

	// webshareDirectFailureCooldown is how long ProxyURL() skips an IP
	// that just hit the failure threshold. Deliberately longer than
	// go-html-proxy's 2-minute connection-failure cooldown: this is
	// backing off from a rate limiter's request-pattern judgment, which
	// plausibly has a longer memory than a transient connection error
	// does. Not empirically tuned against DuckDuckGo's actual recovery
	// window (unknown) -- a reasonable starting point, adjust if the
	// fleet's own data suggests otherwise.
	webshareDirectFailureCooldown = 3 * time.Minute
)

// webshareDirectDefaultBaseURL is a var (not const) so internal tests can
// point it at an httptest server instead of the real Webshare API.
var webshareDirectDefaultBaseURL = "https://proxy.webshare.io/api/v2/proxy/list/"

// webshareDirectEntry is one pool member. addr is the plain host:port
// (no credentials) callers correlate back via ResultReporter.MarkResult --
// see that method's doc comment for why address, not the full proxyURL,
// is the reporting key.
type webshareDirectEntry struct {
	proxyURL string // fully formed http://user:pass@address:port
	addr     string // address:port only

	// failures/cooldownUntil mirror go-html-proxy's webshareproxy.Pool
	// (this session, 2026-08-25) exactly -- same reasoning, same shape,
	// promoted here because this supplier needs it too. See that
	// package's entry doc comment for the full rationale; not repeated
	// here beyond the threshold/duration differences noted on the
	// constants above.
	failures      atomic.Int32
	cooldownUntil atomic.Int64 // UnixNano; 0 means "not in cooldown"
}

func (e *webshareDirectEntry) eligible(nowUnixNano int64) bool {
	if e.failures.Load() < webshareDirectMaxConsecutiveFailures {
		return true
	}
	cd := e.cooldownUntil.Load()
	return cd != 0 && nowUnixNano >= cd
}

// webshareDirectSupplier round-robins every ProxyURL() call across a
// Webshare account's individually-dialable proxy IPs, fetched from
// Webshare's own account API rather than routed through the shared
// p.webshare.io rotating gateway. Deliberately does NOT cache/reuse
// connections per IP the way go-html-proxy's webshare_direct client does
// -- that optimization is for services that want a WARM connection to
// whichever IP they land on; this supplier is for services (search
// scraping against a target with IP-reputation-based rate limiting) that
// specifically want a DIFFERENT exit IP on every single request, which a
// fresh HTTPClient(..., DisableKeepAlives: true) already guarantees once
// ProxyURL() itself round-robins deterministically instead of relying on
// the gateway's opaque per-connection assignment.
//
// Every call to ProxyURL() advances the round-robin cursor -- callers that
// want the SAME exit IP for a batch of related requests should not use
// this supplier for that batch (there is no session/sticky mode here).
//
// Implements ResultReporter (see MarkResult) so a caller that can judge
// per-request quality beyond plain transport success/failure -- e.g. "this
// response was a 200 but the body was a rate-limiter's challenge page",
// which no generic HTTP client layer can know on its own -- can feed that
// judgment back to cool down the specific IP that produced it.
type webshareDirectSupplier struct {
	apiKey          string
	baseURL         string
	refreshInterval time.Duration
	httpClient      *http.Client
	rules           *noProxyRules

	mu   sync.RWMutex
	list []*webshareDirectEntry
	idx  atomic.Uint64

	stop chan struct{}
}

func (s *webshareDirectSupplier) Name() string { return "webshare_direct" }

func (s *webshareDirectSupplier) Bypass(host string) bool {
	if s.rules == nil {
		return false
	}
	return s.rules.Match(host)
}

// ProxyURL returns the next eligible entry in round-robin order, skipping
// any entry currently in cooldown from a recent MarkResult(..., false)
// call. Empty string (no proxy / direct connection) if the account's pool
// is currently empty OR every entry is in cooldown -- callers should treat
// either the same as any other "none" supplier result rather than block.
func (s *webshareDirectSupplier) ProxyURL() string {
	s.mu.RLock()
	list := s.list
	s.mu.RUnlock()

	n := len(list)
	if n == 0 {
		return ""
	}

	now := time.Now().UnixNano()
	start := s.idx.Add(1)
	for i := 0; i < n; i++ {
		e := list[(int(start)+i)%n]
		if e.eligible(now) {
			return e.proxyURL
		}
	}
	// Every entry is currently in cooldown. Unlike go-html-proxy's Pool
	// (which fails open onto the next entry so a caller mid-request always
	// gets SOME proxy), this returns "" -- a caller retrying against a
	// rate limiter is better served going direct or waiting than being
	// handed back an IP already known to be in its penalty window.
	return ""
}

// MarkResult records an application-level verdict for the exit IP that
// handled a request, identified by its address (host:port, no
// credentials -- this is deliberately what a caller can recover cheaply
// via httptrace.ClientTrace's GotConn/ConnectDone hooks reading
// conn.RemoteAddr(), since a proxied request's connection target IS the
// proxy address; the credentials embedded in ProxyURL()'s return value
// are not visible at that layer and are not needed to identify the entry).
//
// ok=false starts (or refreshes) a cooldown window; ok=true clears it
// immediately. Safe to call with an address not currently in the pool
// (e.g. dropped by a list refresh since this request started) -- a no-op
// then, matching webshareproxy.Pool.MarkResult's existing contract.
func (s *webshareDirectSupplier) MarkResult(addr string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.list {
		if e.addr == addr {
			if ok {
				e.failures.Store(0)
				e.cooldownUntil.Store(0)
			} else if e.failures.Add(1) >= webshareDirectMaxConsecutiveFailures {
				e.cooldownUntil.Store(time.Now().Add(webshareDirectFailureCooldown).UnixNano())
			}
			return
		}
	}
}

// newWebshareDirectSupplier constructs the supplier. Returns noneSupplier{}
// only if the API key is missing -- consistent with every other case in
// NewFromConfig, which never returns an error itself. The initial fetch
// runs in the background; see refreshLoop.
func newWebshareDirectSupplier(cfg Config) Supplier {
	if cfg.WebshareAPIKey == "" {
		activeWebshareDirectSupplier.Store(nil)
		return noneSupplier{}
	}
	s := &webshareDirectSupplier{
		apiKey:          cfg.WebshareAPIKey,
		baseURL:         webshareDirectDefaultBaseURL,
		refreshInterval: webshareDirectDefaultRefreshInterval,
		httpClient:      &http.Client{Timeout: webshareDirectDefaultHTTPTimeout},
		rules:           parseNoProxy(cfg.NoProxy),
		stop:            make(chan struct{}),
	}
	activeWebshareDirectSupplier.Store(s)
	// The initial fetch runs in the background rather than blocking here.
	// Traced live 2026-08-26: paginating a real account (~1800 entries,
	// growing) sequentially against Webshare's API took long enough
	// (roughly a second per 100-entry page, so tens of seconds total) to
	// blow through a deploy's health-check grace period when this
	// constructor sat synchronously in a service's startup path --
	// NewFromConfig is typically called from main()/init, so blocking here
	// blocks the entire service from listening at all. ProxyURL() safely
	// returns "" (== no proxy, identical to every other supplier's
	// empty-URL behavior) until the first background fetch completes.
	go s.refreshLoop()
	return s
}

type webshareEntry struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	ProxyAddress string `json:"proxy_address"`
	Port         int    `json:"port"`
	Valid        bool   `json:"valid"`
}

type webshareListResponse struct {
	Count   int             `json:"count"`
	Next    string          `json:"next"`
	Results []webshareEntry `json:"results"`
}

func (s *webshareDirectSupplier) refresh(ctx context.Context) error {
	var all []webshareEntry
	nextURL := fmt.Sprintf("%s?mode=direct&page=1&page_size=%d", s.baseURL, webshareDirectDefaultPageSize)
	for nextURL != "" {
		page, err := s.fetchPage(ctx, nextURL)
		if err != nil {
			return err
		}
		all = append(all, page.Results...)
		nextURL = page.Next
	}

	// Index the outgoing list by address so any entry that survives into
	// the new list carries its failure/cooldown state forward by pointer,
	// same rationale as webshareproxy.Pool.refresh (this session): a list
	// refresh is metadata, not an amnesty for an IP that was just
	// rate-limited.
	s.mu.RLock()
	prev := s.list
	s.mu.RUnlock()
	prevByAddr := make(map[string]*webshareDirectEntry, len(prev))
	for _, e := range prev {
		prevByAddr[e.addr] = e
	}

	list := make([]*webshareDirectEntry, 0, len(all))
	for _, e := range all {
		if !e.Valid {
			continue
		}
		addr := fmt.Sprintf("%s:%d", e.ProxyAddress, e.Port)
		if old, ok := prevByAddr[addr]; ok {
			list = append(list, old)
			continue
		}
		list = append(list, &webshareDirectEntry{
			proxyURL: fmt.Sprintf("http://%s:%s@%s",
				url.QueryEscape(e.Username), url.QueryEscape(e.Password), addr),
			addr: addr,
		})
	}
	if len(list) == 0 {
		return errors.New("proxysupplier: webshare_direct fetched proxy list has zero valid entries")
	}

	s.mu.Lock()
	s.list = list
	s.mu.Unlock()
	return nil
}

func (s *webshareDirectSupplier) fetchPage(ctx context.Context, pageURL string) (*webshareListResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+s.apiKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webshare API returned status %d for %s", resp.StatusCode, pageURL)
	}
	var page webshareListResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode webshare response: %w", err)
	}
	return &page, nil
}

func (s *webshareDirectSupplier) refreshLoop() {
	// The initial fetch itself, not just subsequent ones -- this is what
	// actually makes newWebshareDirectSupplier's early return non-blocking
	// mean anything. It ran the fetch synchronously in the constructor
	// before; moving the goroutine boundary without also moving this call
	// would leave the pool empty until the FIRST ticker fires, silently
	// (webshareDirectDefaultRefreshInterval is 10 minutes by default) --
	// caught by TestWebshareDirect_FetchesAndRoundRobins timing out
	// waiting for a non-empty ProxyURL() during review of this change.
	ctx, cancel := context.WithTimeout(context.Background(), webshareDirectRefreshBudget)
	_ = s.refresh(ctx) // best-effort; ProxyURL() returns "" until a later refresh succeeds
	cancel()

	ticker := time.NewTicker(s.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), webshareDirectRefreshBudget)
			_ = s.refresh(ctx) // best-effort; keep the previous list on failure
			cancel()
		}
	}
}

// AddrFromProxyURL extracts the address:port portion (no credentials) from
// a proxy URL string in the http://user:pass@address:port form ProxyURL()
// returns. Exported so a caller doesn't need to hand-roll url.Parse just to
// get MarkResult's expected key -- e.g. as a fallback when a caller wants
// to report a result but couldn't capture the actual dialed address via
// httptrace for some reason (a same-process retry path, a test double).
// Prefer the real dialed address from httptrace when available: it's what
// actually handled the request, whereas this is only what ProxyURL()
// intended to hand out.
func AddrFromProxyURL(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return ""
	}
	return u.Host
}
