package apikey

import (
	"github.com/baditaflorin/go-common/env"
	"net/http"
	"os"
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
func New() *Client {
	return &Client{
		BaseURL:    env.String("APIKEY_SERVICE_URL", "http://localhost:18021"),
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
