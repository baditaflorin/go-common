package safehttp

import "net/http"

func closeIdleConnections(transport http.RoundTripper) {
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *extrasTransport) CloseIdleConnections()          { closeIdleConnections(t.inner) }
func (t *uaTransport) CloseIdleConnections()              { closeIdleConnections(t.inner) }
func (t *egressAllowlistTransport) CloseIdleConnections() { closeIdleConnections(t.inner) }
func (t *tls12FallbackTransport) CloseIdleConnections() {
	if t.primary != nil {
		t.primary.CloseIdleConnections()
	}
	if t.fallback != nil {
		t.fallback.CloseIdleConnections()
	}
}
