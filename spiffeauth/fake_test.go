package spiffeauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"github.com/baditaflorin/go-common/middleware"
)

// A fake SPIRE: one CA, one JWT authority, SVIDs for any ID, all in process.
type fakeSPIRE struct {
	t      *testing.T
	td     spiffeid.TrustDomain
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	jwtKey *ecdsa.PrivateKey
	kid    string
}

func newFakeSPIRE(t *testing.T, trustDomain string) *fakeSPIRE {
	t.Helper()
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		t.Fatal(err)
	}
	key := genKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake spire ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return &fakeSPIRE{t: t, td: td, ca: ca, caKey: key, jwtKey: genKey(t), kid: "kid-1"}
}

func genKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (f *fakeSPIRE) x509Bundles() x509bundle.Source {
	return x509bundle.NewSet(x509bundle.FromX509Authorities(f.td, []*x509.Certificate{f.ca}))
}

func (f *fakeSPIRE) jwtBundles() jwtbundle.Source {
	return jwtbundle.NewSet(jwtbundle.FromJWTAuthorities(f.td, map[string]crypto.PublicKey{f.kid: f.jwtKey.Public()}))
}

// svid issues an X.509-SVID for id signed by this CA.
func (f *fakeSPIRE) svid(id string) *x509svid.SVID { return f.svidFrom(id, f.ca, f.caKey, time.Hour) }

func (f *fakeSPIRE) svidFrom(id string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, ttl time.Duration) *x509svid.SVID {
	f.t.Helper()
	u, err := url.Parse(id)
	if err != nil {
		f.t.Fatal(err)
	}
	key := genKey(f.t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), URIs: []*url.URL{u},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(ttl),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		f.t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	sid, _ := spiffeid.FromString(id)
	return &x509svid.SVID{ID: sid, Certificates: []*x509.Certificate{leaf}, PrivateKey: key}
}

// token mints a JWT-SVID signed by the fake JWT authority.
func (f *fakeSPIRE) token(sub string, aud []string, ttl time.Duration) string {
	return f.tokenWith(f.jwtKey, f.kid, sub, aud, ttl)
}

func (f *fakeSPIRE) tokenWith(key *ecdsa.PrivateKey, kid, sub string, aud []string, ttl time.Duration) string {
	f.t.Helper()
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: key, KeyID: kid}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		f.t.Fatal(err)
	}
	now := time.Now()
	tok, err := jwt.Signed(sig).Claims(jwt.Claims{
		Subject: sub, Audience: aud, IssuedAt: jwt.NewNumericDate(now.Add(-time.Second)), Expiry: jwt.NewNumericDate(now.Add(ttl)),
	}).Serialize()
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

type svidSource struct{ s *x509svid.SVID }

func (s svidSource) GetX509SVID() (*x509svid.SVID, error) { return s.s, nil }

// recorder is a middleware.IdentityObserver + AuthObserver.
type recorder struct {
	mu        sync.Mutex
	events    []middleware.AuthEvent
	failures  []string
	fallbacks []string
}

func (r *recorder) ObserveAuth(e middleware.AuthEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}
func (r *recorder) ObserveIdentityFailure(s middleware.AuthSource, reason string) {
	r.mu.Lock()
	r.failures = append(r.failures, string(s)+"/"+reason)
	r.mu.Unlock()
}
func (r *recorder) ObserveIdentityFallback(p, reason string) {
	r.mu.Lock()
	r.fallbacks = append(r.fallbacks, p+"/"+reason)
	r.mu.Unlock()
}
func (r *recorder) snapshot() (events []middleware.AuthEvent, failures, fallbacks []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(events, r.events...), append(failures, r.failures...), append(fallbacks, r.fallbacks...)
}

func testConfig(peers ...Peer) Config {
	return Config{
		Mode: ModeDual, Socket: "unix:///tmp/none.sock", TrustDomains: []string{"0exec.com"},
		Peers: peers, JWTAudience: []string{"fleet-fetch-cache"}, Unmapped: UnmappedDeny,
	}
}

// multiBundles serves the X.509 bundles of several fake SPIREs.
type multiBundles []*fakeSPIRE

func (m multiBundles) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	for _, f := range m {
		if f.td == td {
			return f.x509Bundles().GetX509BundleForTrustDomain(td)
		}
	}
	return nil, errors.New("no bundle")
}

func mustReq(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, "/data", nil)
}
