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
//   - "env"           — legacy: reads EXTERNAL_PROXY_URL, then falls back to
//     PROXY_HOST / PROXY_PORT / PROXY_PROTOCOL / PROXY_USERNAME / PROXY_PASSWORD
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

// Config holds the raw proxy configuration values. Populate it from env vars,
// a struct config, or a YAML file — whatever the calling service uses.
type Config struct {
	// Supplier selects the backend: "plain_proxies", "env", "none" / "".
	Supplier string

	// PlainProxies / generic URL-based suppliers
	Host     string
	Port     string
	Username string
	Password string
	Protocol string // defaults to "http"

	// Legacy env-style: full URL takes precedence over Host/Port fields.
	ExternalProxyURL string
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
	}
}

// New reads PROXY_SUPPLIER (and related vars) from the environment and returns
// the matching Supplier. It is a convenience wrapper for EnvConfig + NewFromConfig.
func New() Supplier {
	return NewFromConfig(EnvConfig())
}

// NewFromConfig selects the supplier described by cfg.
// The self-proxy guard is always applied.
func NewFromConfig(cfg Config) Supplier {
	var s Supplier
	switch cfg.Supplier {
	case "plain_proxies":
		rawURL := buildURL("http", cfg.Host, cfg.Port, cfg.Username, cfg.Password)
		if rawURL == "" {
			return noneSupplier{}
		}
		s = &urlSupplier{name: "plain_proxies", rawURL: rawURL}

	case "env":
		rawURL := cfg.ExternalProxyURL
		if rawURL == "" {
			proto := cfg.Protocol
			if proto == "" {
				proto = "http"
			}
			rawURL = buildURL(proto, cfg.Host, cfg.Port, cfg.Username, cfg.Password)
		}
		if rawURL == "" {
			return noneSupplier{}
		}
		s = &urlSupplier{name: "env", rawURL: rawURL}

	default:
		return noneSupplier{}
	}

	if isSelfProxy(s.ProxyURL()) {
		return noneSupplier{}
	}
	return s
}

// HTTPClient returns an *http.Client configured to route through s.
// Returns nil when s is "none" — the caller should use its default client
// (e.g. safehttp) instead.
//
//	client := proxysupplier.HTTPClient(s, 8*time.Second)
//	if client == nil {
//	    client = safehttp.NewClient(...)
//	}
func HTTPClient(s Supplier, timeout time.Duration) *http.Client {
	if s.ProxyURL() == "" {
		return nil
	}
	proxyURL, err := url.Parse(s.ProxyURL())
	if err != nil {
		return nil
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
		},
		Timeout: timeout,
	}
}

// --- implementations --------------------------------------------------------

type noneSupplier struct{}

func (noneSupplier) Name() string     { return "none" }
func (noneSupplier) ProxyURL() string { return "" }

type urlSupplier struct {
	name   string
	rawURL string
}

func (s *urlSupplier) Name() string     { return s.name }
func (s *urlSupplier) ProxyURL() string { return s.rawURL }

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
