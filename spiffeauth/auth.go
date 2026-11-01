package spiffeauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/spiffe"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// The reasons a presented SPIFFE identity is not accepted. They are the values
// of spiffe_verify_failures_total{reason} and nothing else: no token, no
// certificate content and no peer detail ever reaches a log, an error or a metric.
const (
	ReasonInvalidSVID     = "invalid_svid"     // the certificate chain does not verify against the trust bundle
	ReasonUntrustedDomain = "untrusted_domain" // verified, but from a trust domain this service does not accept
	ReasonExpiredToken    = "expired_token"    // JWT-SVID expired
	ReasonWrongAudience   = "wrong_audience"   // JWT-SVID minted for another audience
	ReasonInvalidToken    = "invalid_token"    // JWT-SVID malformed or its signature does not verify
	ReasonTTLTooLong      = "ttl_too_long"     // JWT-SVID valid for longer than SPIFFEAUTH_JWT_MAX_TTL
	ReasonUnmappedID      = "unmapped_id"      // verified, but the ID is not in the peer map
	ReasonNotReady        = "not_ready"        // the Workload API connection is not up yet
	ReasonOther           = "other"
)

// JWTHeader is the request header that can carry a JWT-SVID, for a caller that
// also sends an API key in Authorization.
const JWTHeader = "X-Spiffe-Jwt"

const maxTokenBytes = 8192

// ErrNoServerSVID: the Authenticator has no X.509-SVID of its own to present.
var ErrNoServerSVID = errors.New("spiffeauth: no server X.509-SVID source")

// Authenticator verifies SPIFFE identities against the trust bundle the
// Workload API delivers and maps them to principals. It implements
// middleware.IdentityAuthenticator. Methods are safe for concurrent use.
type Authenticator struct {
	cfg       Config
	peers     *peerMap
	domains   map[string]struct{}
	svid      x509svid.Source
	x509      x509bundle.Source
	jwt       jwtbundle.Source // nil: JWT path off
	now       func() time.Time
	closeFunc func() error
}

var _ middleware.IdentityAuthenticator = (*Authenticator)(nil)

// New connects to the Workload API at cfg.Socket, waits (up to
// cfg.ConnectTimeout) for the first SVID and bundle, and returns the
// Authenticator. It reuses package spiffe's Workload API clients. cfg must be
// enabled (ModeDual) and valid; ConfigFromEnv returns one.
func New(ctx context.Context, cfg Config) (*Authenticator, error) {
	if !cfg.Enabled() {
		return nil, errors.New("spiffeauth: not enabled")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	id, err := spiffe.NewX509Identity(cctx, cfg.Socket)
	if err != nil {
		return nil, errors.New("spiffeauth: the Workload API is unavailable")
	}
	var jwt *spiffe.Source
	var jwtSrc jwtbundle.Source
	if len(cfg.JWTAudience) > 0 {
		if jwt, err = spiffe.New(cctx, cfg.Socket); err != nil {
			_ = id.Close()
			return nil, errors.New("spiffeauth: the Workload API is unavailable")
		}
		jwtSrc = jwt
	}
	a, err := NewFromSources(cfg, id.SVIDSource(), id.BundleSource(), jwtSrc)
	if err != nil {
		_ = id.Close()
		if jwt != nil {
			_ = jwt.Close()
		}
		return nil, err
	}
	a.closeFunc = func() error {
		err := id.Close()
		if jwt != nil {
			if e := jwt.Close(); err == nil {
				err = e
			}
		}
		return err
	}
	return a, nil
}

// NewFromSources builds an Authenticator on explicit sources: svid is this
// workload's own X.509-SVID (needed only for TLSConfig, may be nil), x509 the
// trust bundles for client certificates, jwt the JWT authorities (nil turns the
// JWT path off). New wires the Workload API into it; tests wire fakes.
func NewFromSources(cfg Config, svid x509svid.Source, x509 x509bundle.Source, jwt jwtbundle.Source) (*Authenticator, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Mode != ModeDual {
		return nil, errors.New("spiffeauth: not enabled")
	}
	pm, err := newPeerMap(cfg.Peers, cfg.TrustDomains)
	if err != nil {
		return nil, err
	}
	if x509 == nil {
		return nil, errors.New("spiffeauth: an X.509 bundle source is required")
	}
	if len(cfg.JWTAudience) == 0 {
		jwt = nil
	} else if jwt == nil {
		return nil, errors.New("spiffeauth: a JWT bundle source is required when SPIFFEAUTH_JWT_AUDIENCE is set")
	}
	domains := map[string]struct{}{}
	for _, d := range cfg.TrustDomains {
		domains[d] = struct{}{}
	}
	return &Authenticator{cfg: cfg, peers: pm, domains: domains, svid: svid, x509: x509, jwt: jwt, now: time.Now}, nil
}

// Close releases the Workload API connections.
func (a *Authenticator) Close() error {
	if a == nil || a.closeFunc == nil {
		return nil
	}
	return a.closeFunc()
}

// Config returns the configuration in use (the peer map included; it holds no secret).
func (a *Authenticator) Config() Config { return a.cfg }

// ExpectedPrincipal implements middleware.IdentityAuthenticator.
func (a *Authenticator) ExpectedPrincipal(user string) (string, bool) {
	if a == nil {
		return "", false
	}
	return a.peers.expected(user)
}

func (a *Authenticator) observeFailure(source middleware.AuthSource, reason string) {
	if a.cfg.Observer != nil {
		a.cfg.Observer.ObserveIdentityFailure(source, reason)
	}
}

// TLSConfig is the server-side tls.Config of the mTLS listener: it presents this
// workload's X.509-SVID and asks the client for a certificate. A client that
// sends none is served (the request then goes the API key path); one that sends
// a certificate must present an X.509-SVID that verifies against the trust bundle
// of an accepted trust domain, or the handshake fails. Which principal it is
// decided per request by AuthenticateIdentity, from the peer map.
func (a *Authenticator) TLSConfig() (*tls.Config, error) {
	if a == nil || a.svid == nil {
		return nil, ErrNoServerSVID
	}
	cfg := tlsconfig.TLSServerConfig(a.svid)
	cfg.MinVersion = tls.VersionTLS12
	cfg.ClientAuth = tls.RequestClientCert
	cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return nil
		}
		id, _, err := x509svid.ParseAndVerify(raw, a.x509)
		if err != nil {
			a.observeFailure(middleware.AuthSourceSPIFFEMTLS, ReasonInvalidSVID)
			return errors.New("spiffeauth: client certificate rejected")
		}
		if _, ok := a.domains[id.TrustDomain().Name()]; !ok {
			a.observeFailure(middleware.AuthSourceSPIFFEMTLS, ReasonUntrustedDomain)
			return errors.New("spiffeauth: client certificate rejected")
		}
		return nil
	}
	return cfg, nil
}

// AuthenticateIdentity implements middleware.IdentityAuthenticator.
func (a *Authenticator) AuthenticateIdentity(r *http.Request) middleware.IdentityDecision {
	if a == nil || r == nil {
		return middleware.IdentityDecision{}
	}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		return a.fromCertificates(r.TLS.PeerCertificates)
	}
	if a.jwt != nil {
		if token, viaBearer := jwtFromRequest(r); token != "" {
			return a.fromJWT(token, viaBearer)
		}
	}
	return middleware.IdentityDecision{}
}

func (a *Authenticator) fromCertificates(certs []*x509.Certificate) middleware.IdentityDecision {
	d := middleware.IdentityDecision{Presented: true, Source: middleware.AuthSourceSPIFFEMTLS}
	// The handshake already verified the chain; verify again here so this check
	// does not depend on how the listener was configured.
	id, _, err := x509svid.Verify(certs, a.x509, x509svid.WithTime(a.now()))
	if err != nil {
		d.Reason = ReasonInvalidSVID
		return d
	}
	return a.mapID(d, id)
}

func (a *Authenticator) fromJWT(token string, viaBearer bool) middleware.IdentityDecision {
	d := middleware.IdentityDecision{Presented: true, Source: middleware.AuthSourceSPIFFEJWT, BearerIsSVID: viaBearer}
	if len(token) > maxTokenBytes {
		d.Reason = ReasonInvalidToken
		return d
	}
	svid, err := jwtsvid.ParseAndValidate(token, a.jwt, a.cfg.JWTAudience)
	if err != nil {
		d.Reason = classifyJWTError(err)
		return d
	}
	if svid.Expiry.Sub(a.now()) > a.cfg.JWTMaxTTL {
		d.Reason = ReasonTTLTooLong
		return d
	}
	return a.mapID(d, svid.ID)
}

// mapID applies the trust-domain check and the peer map to a verified ID.
func (a *Authenticator) mapID(d middleware.IdentityDecision, id spiffeid.ID) middleware.IdentityDecision {
	if _, ok := a.domains[id.TrustDomain().Name()]; !ok {
		return a.unmapped(d, ReasonUntrustedDomain)
	}
	peer, ok := a.peers.lookup(id)
	if !ok {
		return a.unmapped(d, ReasonUnmappedID)
	}
	d.Allowed, d.SPIFFEID = true, id.String()
	d.Principal, d.Scope, d.Tier = peer.Principal, peer.Scope, peer.Tier
	return d
}

// unmapped: default deny (403), only ever reached by a caller that presented an SVID.
func (a *Authenticator) unmapped(d middleware.IdentityDecision, reason string) middleware.IdentityDecision {
	d.Reason = reason
	if a.cfg.Unmapped != UnmappedFallback {
		d.Deny, d.Status = true, http.StatusForbidden
	}
	return d
}

// classifyJWTError buckets a go-spiffe error into a reason. The error text is
// read only to pick the bucket and is never kept.
func classifyJWTError(err error) string {
	switch msg := err.Error(); {
	case strings.Contains(msg, "expired"):
		return ReasonExpiredToken
	case strings.Contains(msg, "audience"):
		return ReasonWrongAudience
	}
	return ReasonInvalidToken
}

// jwtFromRequest finds a JWT-SVID: the X-Spiffe-Jwt header, else a bearer that
// has the shape of a JWT (and so is not an ak_/fb_ API key).
func jwtFromRequest(r *http.Request) (token string, viaBearer bool) {
	if v := strings.TrimSpace(r.Header.Get(JWTHeader)); v != "" {
		return v, false
	}
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && looksLikeJWT(v) {
		return v, true
	}
	return "", false
}

func looksLikeJWT(v string) bool {
	return strings.HasPrefix(v, "eyJ") && strings.Count(v, ".") == 2 && !strings.ContainsAny(v, " \t\r\n")
}
