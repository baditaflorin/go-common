package safehttp

import (
	"net/http"
)

// uaTransport injects the configured User-Agent on outbound requests
// that don't already have one set. The earlier shape — relying solely
// on Client.CheckRedirect to set the header — only covered redirect-
// follow requests, so the INITIAL request always went out with Go's
// default "Go-http-client/1.1". That was the silent bug fixed in
// v0.35.0: any fleet caller passing WithUserAgent was being 403'd by
// UA-gating upstreams (Wikidata WDQS T400119 was the canary).
//
// Per-request override semantics: we only inject when the header is
// unset (req.Header.Get returns "" for missing). Callers that set a
// per-request UA via req.Header.Set keep that value — required for
// scrapers / scanners that fingerprint per-call.
//
// Request mutation is via req.Clone — http.RoundTripper contracts
// forbid modifying the caller's *Request, and a shared *Request can
// race with the goroutine that constructed it.
type uaTransport struct {
	inner http.RoundTripper
	ua    string
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ua != "" && req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	return t.inner.RoundTrip(req)
}
