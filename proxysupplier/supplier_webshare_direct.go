package proxysupplier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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

	// webshareDirectMaxRetryAfter caps how long a Retry-After from the
	// Webshare API can postpone the next pool fetch.
	webshareDirectMaxRetryAfter = 5 * time.Minute

	// webshareDirectRefreshJitter spreads the regular refresh by +/- this
	// fraction of the interval, so replicas started together (a rolling
	// deploy, a fleet restart) do not keep hitting the API in the same second.
	webshareDirectRefreshJitter = 0.1
)

// webshareDirectRetryBase / webshareDirectRetryMax shape the retry after a
// FAILED pool fetch (network error, 429, 5xx, undecodable body, zero valid
// entries): the first retry comes after about the base, doubling up to the
// cap, with jitter. Without them a failed initial fetch left the pool empty
// for a whole refresh interval (10 minutes): traced 2026-10-17, one replica of
// five answered 503 egress_unavailable from 12:37 to 12:48 after the Webshare
// API throttled five replicas that restarted within three minutes. Vars so
// tests can shrink them.
var (
	webshareDirectRetryBase = time.Second
	webshareDirectRetryMax  = time.Minute
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

	// Refresh bookkeeping (see refreshLoop and poolSnapshot).
	refreshOK      atomic.Uint64
	refreshFail    atomic.Uint64
	consecFail     atomic.Int32
	lastOKUnixNano atomic.Int64 // 0 = never succeeded

	// retryBase / retryMax are webshareDirectRetryBase / webshareDirectRetryMax as they were at
	// construction (zero = the package defaults).
	retryBase, retryMax time.Duration

	// rand01 returns a float in [0,1); nil uses math/rand/v2. Tests pin it.
	rand01 func() float64

	stop     chan struct{}
	stopOnce sync.Once
}

// Close stops the background refresh loop. Suppliers live for the whole life of a service,
// so production code never needs it; tests and long-running tools that build suppliers
// repeatedly do.
func (s *webshareDirectSupplier) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
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

// ProxyURLForKey deterministically selects a proxy URL for key instead of
// advancing the shared round-robin cursor: the same key always maps to the
// same exit IP, for a caller that wants session/domain affinity (e.g.
// "hold the same IP across several requests to this target") rather than a
// fresh one on every call. Does not touch s.idx, so keyed and unkeyed
// (ProxyURL) callers never distort each other's distribution.
//
// Selection is stable across a list refresh because refresh() stores s.list
// pre-sorted by addr -- hashing key against a canonical order means an
// entry's mapping only shifts when the pool's actual membership changes
// (an IP added or removed), never merely because Webshare's API happened
// to return the same set in a different order on a later fetch.
//
// Falls forward through the same eligible list starting at the hashed
// index, exactly like ProxyURL()'s cooldown-skip -- a key whose preferred
// entry is currently cooling down gets the next eligible one instead of
// blocking. Returns "" under the same empty-pool/all-cooling-down
// conditions ProxyURL() does.
func (s *webshareDirectSupplier) ProxyURLForKey(key string) string {
	s.mu.RLock()
	list := s.list
	s.mu.RUnlock()

	n := len(list)
	if n == 0 {
		return ""
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	start := int(h.Sum32() % uint32(n))

	now := time.Now().UnixNano()
	for i := 0; i < n; i++ {
		e := list[(start+i)%n]
		if e.eligible(now) {
			return e.proxyURL
		}
	}
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
		retryBase:       webshareDirectRetryBase,
		retryMax:        webshareDirectRetryMax,
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

	// Canonical order (by addr) so ProxyURLForKey's hash->index mapping is
	// stable across refreshes without re-sorting on every call — ProxyURL's
	// round-robin is order-agnostic, so this costs it nothing.
	sort.Slice(list, func(i, j int) bool { return list[i].addr < list[j].addr })

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
		return nil, &webshareAPIError{
			Status:     resp.StatusCode,
			URL:        pageURL,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	var page webshareListResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode webshare response: %w", err)
	}
	return &page, nil
}

// webshareAPIError is a non-200 answer from the Webshare account API.
type webshareAPIError struct {
	Status     int
	URL        string
	RetryAfter time.Duration // from the Retry-After header; 0 when absent or unusable
}

func (e *webshareAPIError) Error() string {
	return fmt.Sprintf("webshare API returned status %d for %s", e.Status, e.URL)
}

// parseRetryAfter reads a Retry-After header (delay seconds or an HTTP date).
// Anything unusable is 0; the value is capped at webshareDirectMaxRetryAfter.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	if d < 0 {
		return 0
	}
	return min(d, webshareDirectMaxRetryAfter)
}

func (s *webshareDirectSupplier) random() float64 {
	if s.rand01 != nil {
		return s.rand01()
	}
	return rand.Float64()
}

// nextDelay is how long refreshLoop waits before the next pool fetch.
//
//   - after a success: the refresh interval, spread by +/- webshareDirectRefreshJitter;
//   - after a failure: exponential backoff from webshareDirectRetryBase (doubling per
//     consecutive failure) up to webshareDirectRetryMax and never beyond the refresh
//     interval, with "equal jitter" (half fixed, half random) so replicas that failed
//     together do not retry together, and at least the API's own Retry-After.
func (s *webshareDirectSupplier) nextDelay(err error, consecutiveFailures int) time.Duration {
	if err == nil {
		d := float64(s.refreshInterval)
		return time.Duration(d * (1 + webshareDirectRefreshJitter*(2*s.random()-1)))
	}
	base, ceiling := s.retryBase, s.retryMax
	if base <= 0 {
		base = webshareDirectRetryBase
	}
	if ceiling <= 0 {
		ceiling = webshareDirectRetryMax
	}
	backoff := base
	for i := 1; i < consecutiveFailures && backoff < ceiling; i++ {
		backoff *= 2
	}
	backoff = min(backoff, ceiling, s.refreshInterval)
	d := backoff/2 + time.Duration(float64(backoff/2)*s.random())
	var apiErr *webshareAPIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > d {
		d = apiErr.RetryAfter
	}
	return min(d, max(s.refreshInterval, webshareDirectMaxRetryAfter))
}

// refreshOnce runs one bounded fetch and records the outcome for the metrics.
func (s *webshareDirectSupplier) refreshOnce() error {
	ctx, cancel := context.WithTimeout(context.Background(), webshareDirectRefreshBudget)
	defer cancel()
	err := s.refresh(ctx)
	if err != nil {
		s.refreshFail.Add(1)
		n := s.consecFail.Add(1)
		if n <= 3 || n%10 == 0 {
			log.Printf("proxysupplier: webshare_direct pool refresh failed (%d in a row, pool keeps %d entries): %v", n, s.poolLen(), err)
		}
		return err
	}
	if n := s.consecFail.Swap(0); n > 0 {
		log.Printf("proxysupplier: webshare_direct pool refresh recovered after %d failed attempts (%d entries)", n, s.poolLen())
	}
	s.refreshOK.Add(1)
	s.lastOKUnixNano.Store(time.Now().UnixNano())
	return nil
}

func (s *webshareDirectSupplier) poolLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.list)
}

// refreshLoop keeps the pool fresh. The first fetch happens at once and a failed
// one (the pool is then empty, so ProxyURL() answers "" and an egress that must not
// go direct fails closed) is retried with backoff instead of waiting out a whole
// refresh interval; after a failure with a live pool the previous list stays in use.
func (s *webshareDirectSupplier) refreshLoop() {
	err := s.refreshOnce()
	timer := time.NewTimer(s.nextDelay(err, int(s.consecFail.Load())))
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			err = s.refreshOnce()
			timer.Reset(s.nextDelay(err, int(s.consecFail.Load())))
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
