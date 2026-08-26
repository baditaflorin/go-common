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
)

// webshareDirectDefaultBaseURL is a var (not const) so internal tests can
// point it at an httptest server instead of the real Webshare API.
var webshareDirectDefaultBaseURL = "https://proxy.webshare.io/api/v2/proxy/list/"

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
type webshareDirectSupplier struct {
	apiKey          string
	baseURL         string
	refreshInterval time.Duration
	httpClient      *http.Client
	rules           *noProxyRules

	mu   sync.RWMutex
	list []string // fully formed http://user:pass@address:port entries
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

// ProxyURL returns the next entry in round-robin order. Empty string (no
// proxy / direct connection) only if the account's pool is currently
// empty -- callers should treat that the same as any other "none"
// supplier result.
func (s *webshareDirectSupplier) ProxyURL() string {
	s.mu.RLock()
	list := s.list
	s.mu.RUnlock()

	n := len(list)
	if n == 0 {
		return ""
	}
	i := s.idx.Add(1)
	return list[int(i)%n]
}

// newWebshareDirectSupplier constructs the supplier and performs a
// synchronous initial fetch so ProxyURL() is immediately usable. Returns
// noneSupplier{} (not an error) if the initial fetch fails or the API key
// is missing -- consistent with every other case in NewFromConfig, which
// never returns an error itself.
func newWebshareDirectSupplier(cfg Config) Supplier {
	if cfg.WebshareAPIKey == "" {
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
	if err := s.refresh(context.Background()); err != nil {
		return noneSupplier{}
	}
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

	list := make([]string, 0, len(all))
	for _, e := range all {
		if !e.Valid {
			continue
		}
		list = append(list, fmt.Sprintf("http://%s:%s@%s:%d",
			url.QueryEscape(e.Username), url.QueryEscape(e.Password), e.ProxyAddress, e.Port))
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
