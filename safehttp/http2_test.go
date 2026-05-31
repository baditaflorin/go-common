package safehttp

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// allowLoopbackForTest unblocks 127.0.0.1 / ::1 so the SSRF guard lets
// the test reach a loopback httptest server, restoring the prior
// allowlist on cleanup.
func allowLoopbackForTest(t *testing.T) {
	t.Helper()
	SetAllowedPrivateIPs([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")})
	t.Cleanup(func() { SetAllowedPrivateIPs(nil) })
}

// newH2TestServer starts an httptest TLS server that offers HTTP/2 over
// ALPN, plus a cert pool that trusts its self-signed cert.
func newH2TestServer(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return ts, pool
}

// headProto issues a HEAD against ts through a client built on the
// safehttp base transport for the given options, and returns the
// negotiated ALPN. tlsCfg trusts the test server's cert.
func headProto(t *testing.T, o *options, ts *httptest.Server, pool *x509.CertPool) string {
	t.Helper()
	tr := newBaseTransport(o, nil)
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodHead, ts.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HEAD %s: %v", ts.URL, err)
	}
	defer resp.Body.Close()
	return NegotiatedProtocol(resp)
}

func TestWithForceHTTP2_NegotiatesH2OnHEAD(t *testing.T) {
	allowLoopbackForTest(t)
	ts, pool := newH2TestServer(t)

	got := headProto(t, &options{forceHTTP2: true}, ts, pool)
	if got != "h2" {
		t.Fatalf("WithForceHTTP2 HEAD: NegotiatedProtocol = %q, want \"h2\"", got)
	}
}

// TestWithoutForceHTTP2_ALPNEmpty documents the exact bug WithForceHTTP2
// fixes: the default transport (custom dialer, ForceAttemptHTTP2 unset)
// offers no "h2" in ALPN, so even an HTTP/2-capable origin cannot
// negotiate it and the field is reported empty.
func TestWithoutForceHTTP2_ALPNEmpty(t *testing.T) {
	allowLoopbackForTest(t)
	ts, pool := newH2TestServer(t)

	got := headProto(t, &options{forceHTTP2: false}, ts, pool)
	if got == "h2" {
		t.Fatalf("default transport unexpectedly negotiated h2; want empty (the workaround-requiring case)")
	}
}

func TestNewBaseTransport_ForceHTTP2Flag(t *testing.T) {
	if !newBaseTransport(&options{forceHTTP2: true}, nil).ForceAttemptHTTP2 {
		t.Error("forceHTTP2=true should set Transport.ForceAttemptHTTP2")
	}
	if newBaseTransport(&options{forceHTTP2: false}, nil).ForceAttemptHTTP2 {
		t.Error("forceHTTP2=false should leave Transport.ForceAttemptHTTP2 unset")
	}
}

func TestWithForceHTTP2_SetsOption(t *testing.T) {
	o := &options{}
	WithForceHTTP2()(o)
	if !o.forceHTTP2 {
		t.Error("WithForceHTTP2 did not set forceHTTP2")
	}
}
