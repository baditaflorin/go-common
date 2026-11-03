package middleware

import (
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/env"
	"github.com/baditaflorin/go-common/header"
)

// X-Auth-* trust (ephemeral-credentials program, phase 3 stream "xauth").
//
// The gateway (CT10 nginx) verifies a caller's key and forwards the request with
// X-Auth-User/Scope/Tier from the keystore's answer; TokenAuthKeystore admits a request
// that carries X-Auth-User without asking the keystore again. Without
// KeystoreOpts.TrustedGatewayCIDRs that trust is given to ANY TCP peer (the legacy
// contract every WithKeystoreAuth* option keeps): whoever can reach the container port
// directly (the host's 10.10.10.0/24 address, a published port on 0.0.0.0, a tailnet
// address, a docker-network neighbour) can send X-Auth-User and use the service without a
// key. infrastructure-fetch-cache closed it for itself in 0.3.62 (XAUTH_TRUST_MODE).
// The variables below give every service the same switch without touching its code.
//
//	XAUTH_TRUST_MODE      off (default): today's behavior, no counting, nothing read from the request
//	                      shadow: the same behavior, plus xauth_untrusted_total counts every request
//	                      that carries X-Auth-User from a peer outside XAUTH_TRUSTED_CIDRS and logs
//	                      one line per peer, caller and credential per minute
//	                      enforce: X-Auth-* is trusted only from XAUTH_TRUSTED_CIDRS; from any other
//	                      peer the headers are dropped and the request goes through the ordinary
//	                      JWT-SVID / API key checks (a valid key or JWT-SVID is still served, a
//	                      request with neither gets 401)
//	XAUTH_TRUSTED_CIDRS   comma separated, default 10.10.10.10/32 (the CT10 gateway, the one address
//	                      that forwards verified headers to a service on the 0docker dockerhost)
//
// An explicit KeystoreOpts.TrustedGatewayCIDRs (server.WithKeystoreAuthTierTrustedProxy) always wins:
// the variables are then ignored. An unknown mode is off, and says so in the log. A trusted list that
// parses to nothing falls back to the default address in enforce mode, so a typo can never turn enforce
// into "trust everyone".
const (
	EnvXAuthTrustMode    = "XAUTH_TRUST_MODE"
	EnvXAuthTrustedCIDRs = "XAUTH_TRUSTED_CIDRS"

	XAuthModeOff     = "off"
	XAuthModeShadow  = "shadow"
	XAuthModeEnforce = "enforce"

	// DefaultXAuthTrustedCIDRs is the CT10 nginx gateway: every request that came through it reaches
	// the upstream with that TCP peer address (measured 2026-11-01 for infrastructure-fetch-cache).
	DefaultXAuthTrustedCIDRs = "10.10.10.10/32"
)

var (
	_ = env.Declare(env.Var{Name: EnvXAuthTrustMode, Kind: env.KindString, Default: XAuthModeOff,
		Doc: "Who may vouch for a request with X-Auth-User/Tier/Scope (middleware.TokenAuthKeystore): off (default; the header is trusted from every TCP peer, as before), shadow (same behavior, but requests with X-Auth-User from a peer outside XAUTH_TRUSTED_CIDRS are counted in xauth_untrusted_total and logged), or enforce (the header is trusted only from XAUTH_TRUSTED_CIDRS; any other peer must present an API key or JWT-SVID, else 401). Ignored when the service sets its own trusted-proxy list. Roll back by unsetting it.",
		Check: func(v string) error {
			_, err := parseXAuthMode(v)
			return err
		}})
	_ = env.Declare(env.Var{Name: EnvXAuthTrustedCIDRs, Kind: env.KindList, Default: DefaultXAuthTrustedCIDRs,
		Doc: "TCP peers (CIDRs or addresses, comma separated) whose X-Auth-User/Tier/Scope is trusted when XAUTH_TRUST_MODE is shadow or enforce. Default: the CT10 gateway."})
)

// XAuthObserver is the optional second interface of KeystoreOpts.Observer: it receives one call per request
// that carried X-Auth-User from a peer outside the trusted list, in shadow and enforce mode. credential is
// none, key, jwt or key+jwt (what else the request carried); only none depends on the forgeable header alone.
// promx.AuthCollectors implements it as xauth_untrusted_total{service,mode,src,caller,credential}.
// Implementations must not block.
type XAuthObserver interface {
	ObserveXAuthUntrusted(mode, src, caller, credential string)
}

type xauthTrust struct {
	mode  string
	cidrs []string

	logMu   sync.Mutex
	logSeen map[string]time.Time
}

type xauthErr string

func (e xauthErr) Error() string { return string(e) }

func parseXAuthMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "false", "0", "no", "disabled":
		return XAuthModeOff, nil
	case XAuthModeShadow:
		return XAuthModeShadow, nil
	case XAuthModeEnforce:
		return XAuthModeEnforce, nil
	}
	return "", xauthErr("must be off, shadow or enforce")
}

// loadXAuthTrust reads the environment. getenv is os.Getenv outside tests.
func loadXAuthTrust(getenv func(string) string, logf func(string, ...any)) *xauthTrust {
	x := &xauthTrust{mode: XAuthModeOff}
	mode, err := parseXAuthMode(getenv(EnvXAuthTrustMode))
	if err != nil {
		logf("xauth: unknown %s %q, using off", EnvXAuthTrustMode, getenv(EnvXAuthTrustMode))
		return x
	}
	x.mode = mode
	if mode == XAuthModeOff {
		return x
	}
	raw := strings.TrimSpace(getenv(EnvXAuthTrustedCIDRs))
	if raw == "" {
		raw = DefaultXAuthTrustedCIDRs
	}
	for _, c := range strings.Split(raw, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if p, err := netip.ParsePrefix(c); err == nil {
			x.cidrs = append(x.cidrs, p.String())
			continue
		}
		if a, err := netip.ParseAddr(c); err == nil {
			x.cidrs = append(x.cidrs, netip.PrefixFrom(a, a.BitLen()).String())
			continue
		}
		logf("xauth: ignoring invalid %s entry %q", EnvXAuthTrustedCIDRs, c)
	}
	if len(x.cidrs) == 0 {
		// An empty list would be the legacy trust-everyone contract: never let a bad list
		// turn enforce (or the shadow census) into that.
		x.cidrs = []string{DefaultXAuthTrustedCIDRs}
	}
	logf("xauth: mode=%s trusted_cidrs=%v", x.mode, x.cidrs)
	return x
}

func (x *xauthTrust) active() bool { return x != nil && x.mode != XAuthModeOff }

// credentialOf says what else a request carried besides the X-Auth-* headers.
func xauthCredentialOf(r *http.Request) string {
	key := ExtractToken(r) != ""
	jwt := strings.TrimSpace(r.Header.Get("X-Spiffe-Jwt")) != ""
	switch {
	case key && jwt:
		return "key+jwt"
	case key:
		return "key"
	case jwt:
		return "jwt"
	}
	return "none"
}

// note counts and (rate limited) logs a request that carries X-Auth-User from an untrusted peer.
func (x *xauthTrust) note(r *http.Request, obs XAuthObserver, logf func(string, ...any)) {
	src := r.RemoteAddr
	if i := strings.LastIndexByte(src, ':'); i > 0 && !strings.HasSuffix(src, "]") {
		src = src[:i]
	}
	src = strings.Trim(src, "[]")
	caller := r.Header.Get(header.FleetCaller)
	if len(caller) > 64 {
		caller = caller[:64]
	}
	cred := xauthCredentialOf(r)
	if obs != nil {
		obs.ObserveXAuthUntrusted(x.mode, src, caller, cred)
	}
	key := src + "|" + caller + "|" + cred
	now := time.Now()
	x.logMu.Lock()
	if x.logSeen == nil || len(x.logSeen) > 4096 {
		x.logSeen = map[string]time.Time{}
	}
	if last, ok := x.logSeen[key]; ok && now.Sub(last) < time.Minute {
		x.logMu.Unlock()
		return
	}
	x.logSeen[key] = now
	x.logMu.Unlock()
	user := r.Header.Get(header.AuthUser)
	if len(user) > 64 {
		user = user[:64]
	}
	logf("xauth_untrusted mode=%s src=%q caller=%q credential=%s path=%q x_auth_user=%q", x.mode, src, caller, cred, r.URL.Path, user)
}

// configureXAuth resolves the mode for one TokenAuthKeystore. An explicit trusted list in opts wins and
// disables the variables.
func configureXAuth(opts *KeystoreOpts, logf func(string, ...any)) *xauthTrust {
	if len(opts.TrustedGatewayCIDRs) > 0 {
		return nil
	}
	x := loadXAuthTrust(os.Getenv, logf)
	if x.mode == XAuthModeEnforce {
		opts.TrustedGatewayCIDRs = append([]string(nil), x.cidrs...)
	}
	return x
}
