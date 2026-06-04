package server

import (
	"context"
	"net/http"

	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/safehttp"
)

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

func (d fetchCacheDelegate) FetchGet(ctx context.Context, url string, h http.Header) (*safehttp.FetchResult, error) {
	r, err := d.c.GetWithHeaders(ctx, url, h)
	if err != nil {
		return nil, err
	}
	return &safehttp.FetchResult{Status: r.Status, Header: r.Header, Body: r.Body}, nil
}
