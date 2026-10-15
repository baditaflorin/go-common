package apikey

import (
	"github.com/baditaflorin/go-common/env"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// New returns a Client wired from env vars. AdminToken may be empty if
// the caller only does /verify (no admin endpoints).
//
// The HTTP transport explicitly sets Proxy: nil so HTTP_PROXY/HTTPS_PROXY
// environment variables NEVER route keystore traffic. The keystore is
// always intra-mesh (apikey-service:8080 or host.docker.internal:18021),
// and many fleet services run with HTTPS_PROXY pointed at a public
// residential proxy for recon egress — without this guard, those
// services' /verify calls get tunneled through the public proxy, which
// returns 502 for private addresses and surfaces as
// `ErrKeystoreUnavailable: HTTP 502` from every Verify call. Caught
// live 2026-05-21 across ~28 pentest services. Defense-in-depth
// against operator NO_PROXY omissions in /opt/_shared/proxy.env.
//
// APIKEY_SERVICE_URLS (optional) is an ordered list of keystore nodes, the
// node on this host first (see endpoints.go and ParseEndpointList). When it
// names at least one acceptable endpoint it replaces APIKEY_SERVICE_URL;
// rejected entries are logged and ignored. APIKEY_SERVICE_HEDGE_MS overrides
// the hedge delay (default 300).
func New() *Client {
	base := env.String("APIKEY_SERVICE_URL", "http://localhost:18021")
	var endpoints []string
	if raw := strings.TrimSpace(os.Getenv("APIKEY_SERVICE_URLS")); raw != "" {
		eps, rejected := ParseEndpointList(raw)
		for _, r := range rejected {
			log.Printf("apikey: ignoring APIKEY_SERVICE_URLS entry %q (not a private or fleet-domain keystore URL)", r)
		}
		if len(eps) > 0 {
			endpoints, base = eps, eps[0]
		}
	}
	var hedge time.Duration
	if ms, err := strconv.Atoi(strings.TrimSpace(os.Getenv("APIKEY_SERVICE_HEDGE_MS"))); err == nil && ms > 0 {
		hedge = time.Duration(ms) * time.Millisecond
	}
	return &Client{
		BaseURL:    base,
		Endpoints:  endpoints,
		HedgeDelay: hedge,
		AdminToken: os.Getenv("APIKEY_SERVICE_ADMIN_TOKEN"),
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{Proxy: nil},
		},
		UserAgent: "go-common/apikey",
	}
}

// NewCache returns a Cache wrapping inner with sensible defaults
// (FreshTTL=60s, StaleTTL=15m).
func NewCache(inner Verifier) *Cache {
	return &Cache{
		Inner:    inner,
		FreshTTL: 60 * time.Second,
		StaleTTL: 15 * time.Minute,
		entries:  map[string]cacheEntry{},
	}
}
