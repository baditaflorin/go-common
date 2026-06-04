package safehttp

import (
	"net/http"
)

// tls12FallbackTransport tries the primary (TLS 1.3-default) transport
// first; if the response is a "tls: internal error" style failure, it
// retries with a TLS 1.2-capped transport. This recovers from a real-
// world bug class where servers ALPN-negotiate HTTP/2 on TLS 1.3 but
// then send TLS alert 80 ("internal error") mid-handshake. Idempotent
// — only retries on GET-shaped failures where the request body has
// not been read.
type tls12FallbackTransport struct {
	primary  *http.Transport
	fallback *http.Transport
}

func (t *tls12FallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.primary.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if !isTLSInternalAlert(err) {
		return resp, err
	}
	// Only safe to retry if the body is replayable. For GET/HEAD this
	// is always true. For POST/PUT we'd need GetBody — bail in that case.
	if req.Body != nil && req.GetBody == nil {
		return resp, err
	}
	if req.GetBody != nil {
		nb, gerr := req.GetBody()
		if gerr != nil {
			return resp, err
		}
		req.Body = nb
	}
	return t.fallback.RoundTrip(req)
}
