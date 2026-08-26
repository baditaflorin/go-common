// Package proxysupplier resolves the upstream egress-proxy URL for outbound
// HTTP requests. It is the single source of truth for the proxy-supplier
// factory across the fleet: adding a new provider means adding one case here
// and bumping go-common; every consumer picks it up on the next dep bump.
//
// # Usage
//
//	s := proxysupplier.New()          // reads PROXY_SUPPLIER from env
//	client := proxysupplier.HTTPClient(s, 10*time.Second)
//	if client == nil {
//	    client = safehttp.NewClient(...)  // direct, SSRF-safe
//	}
//
// # Adding a new supplier
//
// 1. Add a case to [New] (or [NewFromConfig] if your service uses a struct config).
// 2. Implement [Supplier] — usually just a [urlSupplier] with the right URL.
// 3. Bump go-common; run fleet-runner update-dep.
//
// # PROXY_SUPPLIER values
//
//   - "plain_proxies" — PlainProxies DC; reads PROXY_HOST / PROXY_PORT /
//     PROXY_USERNAME / PROXY_PASSWORD
//   - "env"           — reads EXTERNAL_PROXY_URL, then falls back to
//     PROXY_HOST / PROXY_PORT / PROXY_PROTOCOL / PROXY_USERNAME / PROXY_PASSWORD
//   - "multi"         — weighted random across a pool; reads PROXY_URLS
//     (comma-separated list of proxy URLs) and optionally PROXY_WEIGHTS
//     (comma-separated integers, same length as PROXY_URLS; defaults to
//     equal weight). Each outbound request independently picks a proxy
//     proportional to its weight. Self-proxy entries are silently dropped.
//     Example:
//     PROXY_SUPPLIER=multi
//     PROXY_URLS=http://u:p@host1:1338,http://u:p@host2:80
//     PROXY_WEIGHTS=70,30
//   - "none" / ""     — direct connection (default)
//
// A self-proxy guard is always applied: if the resolved URL routes back to
// this host (loopback literal or own hostname), [noneSupplier] is returned
// to prevent proxy loops.
package proxysupplier

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Supplier resolves the upstream proxy URL for a single outbound request.
// ProxyURL returns "" for a direct (no-proxy) connection.
type Supplier interface {
	// Name returns the supplier identifier (e.g. "plain_proxies", "env", "none").
	Name() string
	// ProxyURL returns the full proxy URL string, or "" for direct.
	ProxyURL() string
}

// bypasser is an optional capability — Suppliers may implement it to expose
// NO_PROXY-style match rules. HTTPClient consults Bypass(host) before
// returning a proxy URL; if Bypass returns true the request goes direct.
type bypasser interface {
	Bypass(host string) bool
}

// keepAliver is an optional capability — Suppliers built via NewFromConfig
// with Config.KeepAliveEnabled implement it. HTTPClient consults it to
// decide the Transport's DisableKeepAlives setting; a Supplier that does
// not implement this interface (including any pre-existing custom Supplier
// implementation from before this interface existed) gets the original
// unconditional DisableKeepAlives: true behavior, unchanged.
type keepAliver interface {
	KeepAliveEnabled() bool
}

// ResultReporter is an optional capability -- currently implemented only
// by the webshare_direct supplier -- for callers that can judge a
// request's outcome at a level the HTTP transport itself cannot see. A
// transport-level success/failure signal (connection error vs not) can't
// tell "the exit IP got a 200 whose body was a rate limiter's challenge
// page" from a genuinely good response; only the caller inspecting that
// body can. Exported (unlike bypasser/keepAliver, which HTTPClient
// consults internally) because the reporting caller lives in the
// APPLICATION, not this package -- e.g. a search-scraping service that
// parses DDG's response body for bot-detection markers reports back here
// after the fact, in a different repo, well after HTTPClient's own
// Transport.Proxy call already picked and used the entry.
//
// Type-assert the Supplier ResolveSupplier()/NewFromConfig() returned:
//
//	if rr, ok := supplier.(proxysupplier.ResultReporter); ok {
//	    rr.MarkResult(addr, ok)
//	}
//
// addr is the plain host:port a caller recovers via
// httptrace.ClientTrace's GotConn/ConnectDone hooks reading
// conn.RemoteAddr() -- see webshareDirectSupplier.MarkResult's doc comment
// for why that, not ProxyURL()'s full credentialed URL, is the key.
type ResultReporter interface {
	MarkResult(addr string, ok bool)
}

// Config holds the raw proxy configuration values. Populate it from env vars,
// a struct config, or a YAML file — whatever the calling service uses.
type Config struct {
	// Supplier selects the backend: "plain_proxies", "env", "multi", "none" / "".
	Supplier string

	// PlainProxies / generic URL-based suppliers
	Host     string
	Port     string
	Username string
	Password string
	Protocol string // defaults to "http"

	// Legacy env-style: full URL takes precedence over Host/Port fields.
	ExternalProxyURL string

	// Multi-proxy pool (PROXY_SUPPLIER=multi).
	// ProxyURLs is a comma-separated list of proxy URLs.
	// ProxyWeights is an optional comma-separated list of positive integers
	// with the same length as ProxyURLs. Defaults to equal weight when empty.
	ProxyURLs    string
	ProxyWeights string

	// NoProxy: comma-separated list of hosts/domains/CIDRs that bypass the
	// proxy. Matches Go's standard NO_PROXY semantics plus our extensions:
	//
	//   - "localhost" / "127.0.0.1" / "::1"     — exact host match
	//   - ".0crawl.com" / "0crawl.com"          — domain suffix match (with or without leading dot)
	//   - "10.0.0.0/8" / "172.16.0.0/12"        — CIDR match against resolved IP and literal IP
	//   - "host.docker.internal"                — exact host match
	//   - "*"                                   — match all (disables the proxy entirely)
	//
	// Calls to matching hosts go DIRECT (no proxy). Without this, the
	// proxy provider tries to resolve internal docker hostnames externally
	// and fails with target_connect_resolve_failed.
	//
	// Fleet default: ".0crawl.com,.0exec.com,localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,host.docker.internal"
	NoProxy string

	// WebshareAPIKey selects PROXY_SUPPLIER=webshare_direct: round-robins
	// every ProxyURL() call across a Webshare account's individually-
	// dialable proxy IPs (fetched from Webshare's own account API), rather
	// than the shared p.webshare.io rotating gateway's opaque per-
	// connection assignment. This is the account-level API token (Webshare
	// dashboard -> API), distinct from the per-proxy username/password in
	// Host/Port/Username/Password. The fleet materializes this as
	// WEBSHARE_API_KEY alongside the other shared proxy creds.
	//
	// Use this instead of KeepAliveEnabled when the target itself rate-
	// limits by IP reputation (traced live 2026-08-26: DuckDuckGo search
	// began serving bot-detection "anomaly" pages under sustained volume
	// through the shared gateway) -- reusing one warm connection
	// concentrates request volume onto whichever single exit IP that
	// connection landed on, which is the wrong direction for evading an
	// IP-reputation-based limiter. Deterministic round-robin across the
	// full pool spreads load evenly instead.
	WebshareAPIKey string

	// KeepAliveEnabled controls whether HTTPClient's Transport reuses
	// connections. Zero-value false preserves this package's original,
	// unconditional behavior (DisableKeepAlives: true, a fresh proxy
	// connection per request) -- existing callers that construct Config{}
	// directly or via EnvConfig() before this field existed are completely
	// unaffected; only a caller that explicitly reads the new env var (or
	// sets this field) opts in.
	//
	// Why this needs to be a real option, not just a doc note: fresh-
	// connection-per-request is the RIGHT tradeoff for suppliers where
	// rotation is the entire point and the target is lenient about connect
	// latency. It is the WRONG tradeoff for a strained/rate-limited proxy
	// pool, where every request paying a full TCP+TLS+CONNECT handshake
	// turns ordinary pool congestion into request-level timeouts -- traced
	// live 2026-08-26: go-search-duck-go's DDG search requests were
	// failing with "context deadline exceeded" against the shared Webshare
	// gateway not because the gateway was fully down (a direct probe from
	// the same egress path succeeded 2 of 3 tries), but because every
	// single search paid a fresh handshake into an already-congested pool.
	// go-html-proxy's webshare_direct mode solved the identical problem
	// for its own traffic by keeping per-IP connections warm; this field
	// is the equivalent knob for services still on PROXY_SUPPLIER=env that
	// choose to accept "a few connections see more traffic" in exchange
	// for not re-paying handshake cost on every request.
	KeepAliveEnabled bool
}

// EnvConfig builds a Config by reading the canonical fleet env vars.
// Call this in your main() or factory and pass the result to NewFromConfig.
//
//	cfg := proxysupplier.EnvConfig()
//	s   := proxysupplier.NewFromConfig(cfg)
func EnvConfig() Config {
	return Config{
		Supplier:         strings.ToLower(strings.TrimSpace(os.Getenv("PROXY_SUPPLIER"))),
		Host:             os.Getenv("PROXY_HOST"),
		Port:             os.Getenv("PROXY_PORT"),
		Username:         os.Getenv("PROXY_USERNAME"),
		Password:         os.Getenv("PROXY_PASSWORD"),
		Protocol:         os.Getenv("PROXY_PROTOCOL"),
		ExternalProxyURL: os.Getenv("EXTERNAL_PROXY_URL"),
		ProxyURLs:        os.Getenv("PROXY_URLS"),
		ProxyWeights:     os.Getenv("PROXY_WEIGHTS"),
		NoProxy:          firstNonEmpty(os.Getenv("NO_PROXY"), os.Getenv("no_proxy")),
		KeepAliveEnabled: strings.EqualFold(strings.TrimSpace(os.Getenv("PROXY_KEEPALIVE_ENABLED")), "true"),
		WebshareAPIKey:   os.Getenv("WEBSHARE_API_KEY"),
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// HTTPClient returns an *http.Client configured to route through s.
// Returns nil when s is "none" — the caller should use its default client
// (e.g. safehttp) instead.
//
// For multi-proxy suppliers the Proxy function is evaluated per-request so
// each outbound call independently draws from the weighted pool.
//
// Keep-alives are disabled on the returned transport so that each outbound
// request opens a fresh TCP connection to the proxy. This is required for
// rotating-IP endpoints (e.g. PlainProxies "-ttl-0", BrightData per-request
// sessions) to actually rotate — pooled connections pin the upstream proxy
// to a single exit IP for the lifetime of the connection.
//
//	client := proxysupplier.HTTPClient(s, 8*time.Second)
//	if client == nil {
//	    client = safehttp.NewClient(...)
//	}
func HTTPClient(s Supplier, timeout time.Duration) *http.Client {
	if s.ProxyURL() == "" {
		return nil
	}
	// Capture optional capabilities once — cheaper than asserting every request.
	bp, _ := s.(bypasser)
	ka, _ := s.(keepAliver)
	// Fresh TCP per request by default — required for rotating-IP endpoints
	// to actually rotate: the proxy gateway routes each new CONNECT tunnel
	// through a different exit IP. A Supplier built with
	// Config.KeepAliveEnabled opts out of that per request in exchange for
	// not re-paying handshake cost against a congested pool — see
	// Config.KeepAliveEnabled's doc comment for when that tradeoff is right.
	disableKeepAlives := true
	if ka != nil && ka.KeepAliveEnabled() {
		disableKeepAlives = false
	}
	return &http.Client{
		Transport: &http.Transport{
			// Proxy is called per-request; single-URL suppliers always return
			// the same URL, multi-proxy suppliers pick a weighted-random one.
			// NO_PROXY-style bypass: if the supplier's bypass rules match
			// the request host, return nil (direct connection) — critical
			// for intra-fleet calls that the proxy provider cannot resolve.
			Proxy: func(req *http.Request) (*url.URL, error) {
				if bp != nil && bp.Bypass(req.URL.Hostname()) {
					return nil, nil
				}
				raw := s.ProxyURL()
				if raw == "" {
					return nil, nil
				}
				return url.Parse(raw)
			},
			DisableKeepAlives: disableKeepAlives,
		},
		Timeout: timeout,
	}
}

// --- implementations --------------------------------------------------------

// --- multiSupplier ----------------------------------------------------------

// multiEntry is a single proxy in the weighted pool.
type multiEntry struct {
	rawURL string
	weight int
}

// newMultiSupplier parses proxyURLs (comma-separated) and weights
// (comma-separated integers; optional). Entries that resolve to the current
// host are silently dropped. Returns nil if the resulting pool is empty.
func newMultiSupplier(proxyURLs, proxyWeights string) *multiSupplier {
	if proxyURLs == "" {
		return nil
	}
	rawURLs := splitTrim(proxyURLs, ',')
	weights := splitTrim(proxyWeights, ',')

	entries := make([]multiEntry, 0, len(rawURLs))
	for i, raw := range rawURLs {
		if raw == "" || isSelfProxy(raw) {
			continue
		}
		w := 1
		if i < len(weights) {
			if v, err := strconv.Atoi(weights[i]); err == nil && v > 0 {
				w = v
			}
		}
		entries = append(entries, multiEntry{rawURL: raw, weight: w})
	}
	if len(entries) == 0 {
		return nil
	}
	total := 0
	for _, e := range entries {
		total += e.weight
	}
	return &multiSupplier{entries: entries, total: total}
}

func splitTrim(s string, sep rune) []string {
	parts := strings.Split(s, string(sep))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// --- helpers ----------------------------------------------------------------

func buildURL(protocol, host, port, username, password string) string {
	if host == "" {
		return ""
	}
	if protocol == "" {
		protocol = "http"
	}
	if username != "" && password != "" {
		return fmt.Sprintf("%s://%s:%s@%s:%s/", protocol, username, password, host, port)
	}
	return fmt.Sprintf("%s://%s:%s/", protocol, host, port)
}

// isSelfProxy returns true when rawURL would route back to this process.
// Checks loopback literals, own hostname, and DNS resolution.
func isSelfProxy(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, lb := range []string{"localhost", "127.0.0.1", "::1", "0.0.0.0"} {
		if host == lb {
			return true
		}
	}
	if h, err := os.Hostname(); err == nil && strings.EqualFold(host, h) {
		return true
	}
	if addrs, err := net.LookupHost(host); err == nil {
		for _, addr := range addrs {
			if ip := net.ParseIP(addr); ip != nil && ip.IsLoopback() {
				return true
			}
		}
	}
	return false
}
