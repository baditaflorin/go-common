package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/safehttp"
)

// fetchCacheHopHeader marks a GET that is already being served on behalf
// of the fleet fetch-cache. fetchCacheDelegate stamps it on every call it
// makes to the cache; the server's wrapDefaults turns an inbound request
// carrying it into a WithoutFetchCacheContext, so any downstream safehttp
// GET issued while handling that request goes direct to origin instead of
// recursing back into the cache. This bounds any accidental cache->cache
// recursion (most importantly the fetch-cache fetching its own upstreams
// when FLEET_FETCH_CACHE_URL points at itself) to a single hop and fails
// open to direct egress. Without it such a misconfiguration self-recurses
// until the request times out.
const fetchCacheHopHeader = "X-Fetch-Cache-Hop"

// fetchCacheDelegate adapts a fleetfetch.Client to the
// safehttp.FetchDelegate interface. It lives in the server package
// (which already imports both safehttp and fleetfetch) so that
// safehttp itself never imports fleetfetch — fleetfetch imports
// safehttp for its SSRF-safe fallback, and the reverse import would
// create a cycle.
//
// On a fleetfetch error (cache unreachable AND its own direct fallback
// failed) we return the error, which safehttp's extrasTransport treats
// as "fall through to direct egress" — so a cache outage degrades to
// the pre-cache behavior rather than failing the request.
type fetchCacheDelegate struct{ c *fleetfetch.Client }

func (d fetchCacheDelegate) FetchGet(ctx context.Context, target string, h http.Header) (*safehttp.FetchResult, error) {
	// Skip-self: a request that already targets the cache host must not be
	// routed through the cache (that would wrap a cache call in the cache).
	// Returning an error makes extrasTransport fall through to direct egress.
	if targetIsCacheHost(target) {
		return nil, fmt.Errorf("fetch-cache: target %q is the cache host; serving direct", target)
	}
	r, err := d.c.GetWithHeaders(ctx, target, withHopHeader(h))
	if err != nil {
		return nil, err
	}
	return &safehttp.FetchResult{Status: r.Status, Header: r.Header, Body: r.Body}, nil
}

// withHopHeader returns a copy of h carrying the one-hop marker, never
// mutating the caller's header map. A nil h yields a fresh single-entry set.
func withHopHeader(h http.Header) http.Header {
	hh := h.Clone()
	if hh == nil {
		hh = http.Header{}
	}
	hh.Set(fetchCacheHopHeader, "1")
	return hh
}

// targetIsCacheHost reports whether target's host equals the configured
// fleet fetch-cache host (FLEET_FETCH_CACHE_URL). Best-effort: any parse
// failure or unset env returns false so the normal path handles it.
func targetIsCacheHost(target string) bool {
	cacheURL := os.Getenv(fleetfetch.EnvCacheURL)
	if cacheURL == "" {
		return false
	}
	cu, err := url.Parse(cacheURL)
	if err != nil || cu.Hostname() == "" {
		return false
	}
	tu, err := url.Parse(target)
	if err != nil {
		return false
	}
	return strings.EqualFold(tu.Hostname(), cu.Hostname())
}
