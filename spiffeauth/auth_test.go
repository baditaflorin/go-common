package spiffeauth

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/middleware"
)

const (
	idAgentTools = "spiffe://0exec.com/ns/fleet/sa/agent-tools"
	idCrawl      = "spiffe://0exec.com/ns/0crawl/sa/go-url-fetcher"
	idStranger   = "spiffe://0exec.com/ns/fleet/sa/stranger"
)

func peers() []Peer {
	return []Peer{
		{ID: "spiffe://0exec.com/ns/*/sa/agent-tools", Principal: "agent-tools", Tier: "internal", Scope: "write"},
		{ID: idCrawl, Principal: "go-url-fetcher", Scope: "read"},
	}
}

// keyVerifier is the keystore stand-in; it records every token it was asked about.
type keyVerifier struct {
	keys map[string]string
	seen []string
}

func (k *keyVerifier) Verify(_ context.Context, key string) (*apikey.VerifyResult, error) {
	k.seen = append(k.seen, key)
	if u, ok := k.keys[key]; ok {
		return &apikey.VerifyResult{User: u, Scope: "read", Tier: "free"}, nil
	}
	return nil, apikey.ErrInvalidKey
}

// rig serves TokenAuthKeystore behind the authenticator's own mTLS config, over a real TCP listener.
type rig struct {
	a    *Authenticator
	rec  *recorder
	keys *keyVerifier
	url  string
	f    *fakeSPIRE
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	f := newFakeSPIRE(t, "0exec.com")
	rec := &recorder{}
	cfg.Observer = rec
	a, err := NewFromSources(cfg, svidSource{f.svid("spiffe://0exec.com/ns/fleet/sa/server")}, f.x509Bundles(), f.jwtBundles())
	if err != nil {
		t.Fatal(err)
	}
	keys := &keyVerifier{keys: map[string]string{"ak_good": "agent-tools", "ak_other": "someone-else"}}
	mw := middleware.TokenAuthKeystore(middleware.KeystoreOpts{Verifier: keys, Observer: rec, Identity: a})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get(header.AuthUser)+"|"+r.Header.Get(header.AuthScope)+"|"+r.Header.Get(header.AuthTier)+"|"+middleware.SPIFFEIDFromContext(r.Context()))
	}))
	tlsCfg, err := a.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(tls.NewListener(ln, tlsCfg)) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &rig{a: a, rec: rec, keys: keys, url: "https://" + ln.Addr().String(), f: f}
}

// client returns an HTTP client that verifies the server's SVID; with svid != nil it presents it.
func (g *rig) client(t *testing.T, id string) *http.Client {
	t.Helper()
	var cfg *tls.Config
	if id == "" {
		cfg = tlsconfig.TLSClientConfig(g.f.x509Bundles(), tlsconfig.AuthorizeAny())
	} else {
		cfg = tlsconfig.MTLSClientConfig(svidSource{g.f.svid(id)}, g.f.x509Bundles(), tlsconfig.AuthorizeAny())
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
}

func get(t *testing.T, c *http.Client, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/data", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMTLS_MappedSVIDAuthenticatesAsPrincipal(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	for _, id := range []string{idAgentTools, "spiffe://0exec.com/ns/0crawl/sa/agent-tools"} {
		code, body := get(t, g.client(t, id), g.url, map[string]string{header.AuthUser: "forged", header.AuthScope: "admin"})
		if code != 200 || body != "agent-tools|write|internal|"+id {
			t.Fatalf("%s: code=%d body=%q", id, code, body)
		}
	}
	code, body := get(t, g.client(t, idCrawl), g.url, nil)
	if code != 200 || body != "go-url-fetcher|read||"+idCrawl {
		t.Fatalf("code=%d body=%q", code, body)
	}
	events, failures, fallbacks := g.rec.snapshot()
	for _, e := range events {
		if e.Source != middleware.AuthSourceSPIFFEMTLS || e.Result != middleware.AuthResultAllow {
			t.Fatalf("event %+v", e)
		}
	}
	if len(events) != 3 || len(failures) != 0 || len(fallbacks) != 0 || len(g.keys.seen) != 0 {
		t.Fatalf("events=%d failures=%v fallbacks=%v keystore=%v", len(events), failures, fallbacks, g.keys.seen)
	}
}

func TestMTLS_UnmappedIDIsDeniedOnlyBecauseItPresentedAnSVID(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	code, _ := get(t, g.client(t, idStranger), g.url, map[string]string{header.APIKey: "ak_good"})
	if code != http.StatusForbidden {
		t.Fatalf("an unmapped SVID must be refused, got %d", code)
	}
	_, failures, _ := g.rec.snapshot()
	if len(failures) != 1 || failures[0] != "spiffe-mtls/unmapped_id" {
		t.Fatalf("failures = %v", failures)
	}
	if len(g.keys.seen) != 0 {
		t.Fatal("the key path must not run after a default deny")
	}
	// The same service without a certificate and with a key is untouched (dual mode).
	code, body := get(t, g.client(t, ""), g.url, map[string]string{header.APIKey: "ak_good"})
	if code != 200 || !strings.HasPrefix(body, "agent-tools|read|free|") {
		t.Fatalf("API key path: code=%d body=%q", code, body)
	}
}

func TestMTLS_UnmappedFallbackPolicyUsesTheKey(t *testing.T) {
	cfg := testConfig(peers()...)
	cfg.Unmapped = UnmappedFallback
	g := newRig(t, cfg)
	code, body := get(t, g.client(t, idStranger), g.url, map[string]string{header.APIKey: "ak_good"})
	if code != 200 || !strings.HasPrefix(body, "agent-tools|read|free|") {
		t.Fatalf("code=%d body=%q", code, body)
	}
	_, failures, fallbacks := g.rec.snapshot()
	if len(failures) != 1 || len(fallbacks) != 1 || fallbacks[0] != "agent-tools/unmapped_id" {
		t.Fatalf("failures=%v fallbacks=%v", failures, fallbacks)
	}
	// No key with it: 401 naming the identity, not "missing token".
	if code, _ := get(t, g.client(t, idStranger), g.url, nil); code != http.StatusUnauthorized {
		t.Fatalf("code=%d", code)
	}
}

func TestMTLS_NoCertificateGoesTheAPIKeyPathAndCountsTheFallback(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	code, body := get(t, g.client(t, ""), g.url, map[string]string{header.APIKey: "ak_good"})
	if code != 200 || !strings.HasPrefix(body, "agent-tools|read|free|") {
		t.Fatalf("code=%d body=%q", code, body)
	}
	if code, _ := get(t, g.client(t, ""), g.url, map[string]string{header.APIKey: "ak_other"}); code != 200 {
		t.Fatalf("code=%d", code)
	}
	if code, _ := get(t, g.client(t, ""), g.url, nil); code != http.StatusUnauthorized {
		t.Fatalf("no credential at all: code=%d", code)
	}
	events, failures, fallbacks := g.rec.snapshot()
	if len(fallbacks) != 1 || fallbacks[0] != "agent-tools/no_svid" {
		t.Fatalf("only the mapped principal's key use is a fallback: %v", fallbacks)
	}
	if len(failures) != 0 || events[0].Source != middleware.AuthSourceKeystore {
		t.Fatalf("failures=%v first source=%s", failures, events[0].Source)
	}
}

func TestMTLS_CertificateFromAnotherCAFailsTheHandshake(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	rogue := newFakeSPIRE(t, "0exec.com") // same trust domain name, different CA
	cfg := tlsconfig.MTLSClientConfig(svidSource{rogue.svid(idAgentTools)}, g.f.x509Bundles(), tlsconfig.AuthorizeAny())
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	if _, err := c.Get(g.url + "/data"); err == nil {
		t.Fatal("a certificate from a CA outside the bundle must not complete a handshake")
	}
	_, failures, _ := g.rec.snapshot()
	if len(failures) == 0 || failures[0] != "spiffe-mtls/invalid_svid" {
		t.Fatalf("failures = %v", failures)
	}
}

func TestMTLS_ForeignTrustDomainIsRefused(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	other := newFakeSPIRE(t, "evil.example")
	// Make the server trust evil.example's CA too, so only the domain allowlist can refuse it.
	a, err := NewFromSources(testConfig(peers()...), svidSource{g.f.svid("spiffe://0exec.com/ns/fleet/sa/server")},
		multiBundles{g.f, other}, g.f.jwtBundles())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg.Observer = g.rec
	cfg, _ := a.TLSConfig()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(tls.NewListener(ln, cfg)) }()
	defer srv.Close()
	ccfg := tlsconfig.MTLSClientConfig(svidSource{other.svid("spiffe://evil.example/ns/fleet/sa/agent-tools")}, g.f.x509Bundles(), tlsconfig.AuthorizeAny())
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: ccfg}, Timeout: 5 * time.Second}
	if _, err := c.Get("https://" + ln.Addr().String()); err == nil {
		t.Fatal("a trust domain outside SPIFFEAUTH_TRUST_DOMAINS must be refused")
	}
	_, failures, _ := g.rec.snapshot()
	if len(failures) == 0 || failures[len(failures)-1] != "spiffe-mtls/untrusted_domain" {
		t.Fatalf("failures = %v", failures)
	}
}

func TestMTLS_AuthenticateIdentityDoesNotTrustTheHandshakeAlone(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	rogue := newFakeSPIRE(t, "0exec.com")
	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: rogue.svid(idAgentTools).Certificates}
	d := g.a.AuthenticateIdentity(req)
	if !d.Presented || d.Allowed || d.Reason != ReasonInvalidSVID || d.Deny {
		t.Fatalf("decision = %+v", d)
	}
}

func TestJWT_Matrix(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	aud := []string{"fleet-fetch-cache"}
	c := g.client(t, "")
	f := g.f
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

	// Valid, by Authorization bearer and by header.
	for _, h := range []map[string]string{bearer(f.token(idAgentTools, aud, 5*time.Minute)), {JWTHeader: f.token(idAgentTools, aud, 5*time.Minute)}} {
		code, body := get(t, c, g.url, h)
		if code != 200 || body != "agent-tools|write|internal|"+idAgentTools {
			t.Fatalf("code=%d body=%q", code, body)
		}
	}
	events, _, _ := g.rec.snapshot()
	if events[0].Source != middleware.AuthSourceSPIFFEJWT || len(g.keys.seen) != 0 {
		t.Fatalf("source=%s keystore=%v", events[0].Source, g.keys.seen)
	}

	cases := []struct {
		name   string
		tok    string
		reason string
	}{
		{"wrong audience", f.token(idAgentTools, []string{"somebody-else"}, 5*time.Minute), ReasonWrongAudience},
		{"expired", f.token(idAgentTools, aud, -time.Minute), ReasonExpiredToken},
		{"lifetime above the cap", f.token(idAgentTools, aud, time.Hour), ReasonTTLTooLong},
		{"signed by an unknown key", f.tokenWith(genKey(t), f.kid, idAgentTools, aud, 5*time.Minute), ReasonInvalidToken},
		{"unknown key id", f.tokenWith(f.jwtKey, "kid-9", idAgentTools, aud, 5*time.Minute), ReasonInvalidToken},
		{"garbage", "eyJhbGciOiJFUzI1NiJ9.e30.AAAA", ReasonInvalidToken},
	}
	for _, tc := range cases {
		before := len(g.keys.seen)
		// No key: 401, and the token never reaches the keystore.
		if code, _ := get(t, c, g.url, bearer(tc.tok)); code != http.StatusUnauthorized {
			t.Fatalf("%s: code=%d", tc.name, code)
		}
		if len(g.keys.seen) != before {
			t.Fatalf("%s: the JWT-SVID was sent to the keystore", tc.name)
		}
		// With a key: the key decides (dual mode), and the failure and fallback are counted.
		hdr := bearer(tc.tok)
		hdr[header.APIKey] = "ak_good"
		if code, _ := get(t, c, g.url, hdr); code != 200 {
			t.Fatalf("%s with key: code=%d", tc.name, code)
		}
		_, failures, fallbacks := g.rec.snapshot()
		want := "spiffe-jwt/" + tc.reason
		if !contains(failures, want) || !contains(fallbacks, "agent-tools/"+tc.reason) {
			t.Fatalf("%s: failures=%v fallbacks=%v want %s", tc.name, failures, fallbacks, want)
		}
	}
}

func TestJWT_UnmappedIDIsDenied(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	tok := g.f.token(idStranger, []string{"fleet-fetch-cache"}, 5*time.Minute)
	if code, _ := get(t, g.client(t, ""), g.url, map[string]string{"Authorization": "Bearer " + tok, header.APIKey: "ak_good"}); code != http.StatusForbidden {
		t.Fatalf("code=%d", code)
	}
}

func TestJWT_OffWithoutAudienceAndAPIKeysAreNotMistakenForJWTs(t *testing.T) {
	cfg := testConfig(peers()...)
	cfg.JWTAudience = nil
	g := newRig(t, cfg)
	tok := g.f.token(idAgentTools, []string{"fleet-fetch-cache"}, 5*time.Minute)
	// JWT path off: a JWT-shaped bearer is an ordinary (invalid) key for the keystore, as before.
	if code, _ := get(t, g.client(t, ""), g.url, map[string]string{"Authorization": "Bearer " + tok}); code != http.StatusUnauthorized {
		t.Fatalf("code=%d", code)
	}
	if len(g.keys.seen) != 1 || g.keys.seen[0] != tok {
		t.Fatal("with the JWT path off the bearer must go to the keystore exactly as before")
	}
	// With it on, an API key in Authorization is never parsed as a JWT.
	g2 := newRig(t, testConfig(peers()...))
	if code, _ := get(t, g2.client(t, ""), g2.url, map[string]string{"Authorization": "Bearer ak_good"}); code != 200 {
		t.Fatalf("code=%d", code)
	}
	_, failures, _ := g2.rec.snapshot()
	if len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
}

func TestNothingSecretInFailuresOrErrors(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	tok := g.f.token(idAgentTools, []string{"other"}, 5*time.Minute)
	d := g.a.AuthenticateIdentity(func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/data", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return r
	}())
	if d.Reason != ReasonWrongAudience || d.Principal != "" || d.SPIFFEID != "" {
		t.Fatalf("decision = %+v", d)
	}
	_, failures, _ := g.rec.snapshot()
	for _, s := range failures {
		if strings.Contains(s, tok[:20]) || len(s) > 64 {
			t.Fatalf("failure label %q carries request data", s)
		}
	}
}

func TestExpectedPrincipalIsConfigOnly(t *testing.T) {
	g := newRig(t, testConfig(peers()...))
	if p, ok := g.a.ExpectedPrincipal("agent-tools"); !ok || p != "agent-tools" {
		t.Fatal("mapped principal not expected")
	}
	if _, ok := g.a.ExpectedPrincipal("someone-else"); ok {
		t.Fatal("unmapped principal expected")
	}
}

func TestTLSConfigNeedsAnSVID(t *testing.T) {
	f := newFakeSPIRE(t, "0exec.com")
	a, err := NewFromSources(testConfig(peers()...), nil, f.x509Bundles(), f.jwtBundles())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.TLSConfig(); err != ErrNoServerSVID {
		t.Fatalf("err = %v", err)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
