package fleetfetch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/go-common/env"
)

// SPIFFE identity for the call to the cache (Phase 3 of the ephemeral-credentials
// program, fleet-state docs/architecture/ephemeral-credentials).
//
// DUAL MODE. A client that has an identity source sends an audience-scoped
// JWT-SVID in the X-Spiffe-Jwt header NEXT TO its API key, never instead of it. A
// cache with SPIFFEAUTH_MODE=dual (go-common spiffeauth) that accepts the JWT-SVID
// authenticates the call as the mapped principal without asking the keystore; a
// cache that does not (an older one, or one with the mode off) ignores the
// header and decides by the key, exactly as before. So a failure anywhere on the
// identity path (no Workload API, no entry, an expired token) costs nothing but
// the keystore round trip the key path always had. No request is ever sent
// without its key because an identity exists, and none is withheld.
//
// Off by default. server.New installs the source only when FLEET_FETCH_CACHE_SPIFFE=dual
// (see server/fleetfetchid.go); a fleetfetch user that never calls
// SetDefaultIdentity or WithIdentity sends exactly what it sent before, and emits
// no Event.Identity.

// EnvSPIFFE selects the SPIFFE identity path of every fleetfetch client in the
// process: "off" (default, also unset) or "dual". Rollback: unset it, restart.
const EnvSPIFFE = "FLEET_FETCH_CACHE_SPIFFE"

// EnvSPIFFEAudience overrides the JWT-SVID audience, which must be one the cache
// lists in SPIFFEAUTH_JWT_AUDIENCE. Default DefaultSPIFFEAudience.
const EnvSPIFFEAudience = "FLEET_FETCH_CACHE_SPIFFE_AUDIENCE"

// DefaultSPIFFEAudience is the SPIFFE ID of the fetch cache, the audience of the
// JWT-SVID a caller presents to it.
const DefaultSPIFFEAudience = "spiffe://0exec.com/ns/fleet/sa/infrastructure-fetch-cache"

func init() {
	_ = env.Declare(env.Var{Name: EnvSPIFFE, Kind: env.KindString, Default: "off",
		Doc: "SPIFFE identity for calls to the fetch cache (fleetfetch, wired by server.New): off (default, no change) or dual (every cache call also carries an audience-scoped JWT-SVID in X-Spiffe-Jwt next to the API key, which is always sent; needs SPIFFE_ENDPOINT_SOCKET and a SPIRE entry for the workload; without either the call goes on the key alone). Roll back by unsetting it.",
		Check: func(v string) error {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "", "off", "dual":
				return nil
			}
			return errors.New("not off or dual; the API key alone is used")
		}})
	_ = env.Declare(env.Var{Name: EnvSPIFFEAudience, Kind: env.KindString, Default: DefaultSPIFFEAudience,
		Doc: "Audience of the JWT-SVID sent to the fetch cache; must be one of the cache's SPIFFEAUTH_JWT_AUDIENCE."})
}

// SPIFFEJWTHeader carries the JWT-SVID (same name as spiffeauth.JWTHeader).
const SPIFFEJWTHeader = "X-Spiffe-Jwt"

// Identity outcomes in Event.Identity. Empty means the identity path is not enabled.
const (
	// IdentitySPIFFE: the request carried a JWT-SVID (and its key, if any).
	IdentitySPIFFE = "spiffe"
	// IdentityUnavailable: the path is enabled but no JWT-SVID could be had
	// (Workload API down, no entry for this workload): the request went on its key alone.
	IdentityUnavailable = "unavailable"
)

// IdentitySource mints a JWT-SVID for one audience. *spiffe.Source implements it.
type IdentitySource interface {
	Token(ctx context.Context, audience string) (string, error)
}

const (
	identityFetchTimeout = 250 * time.Millisecond // the Workload API is a local socket
	identityMinLife      = 90 * time.Second       // refresh when less than this is left of the 5 m token
	identityRetryAfter   = 20 * time.Second       // after a failure, do not ask again for this long
)

type identity struct {
	src      IdentitySource
	audience string

	mu        sync.Mutex
	token     string
	exp       time.Time
	failUntil time.Time
}

var defaultIdentity atomic.Pointer[identity]

// SetDefaultIdentity installs the process-wide identity source used by every
// Client without its own (WithIdentity). A nil src removes it. audience empty
// means EnvSPIFFEAudience, else DefaultSPIFFEAudience.
func SetDefaultIdentity(src IdentitySource, audience string) {
	if src == nil {
		defaultIdentity.Store(nil)
		return
	}
	defaultIdentity.Store(newIdentity(src, audience))
}

// WithIdentity gives this client its own identity source (tests, or a service
// that manages the Workload API connection itself). It overrides the process default.
func WithIdentity(src IdentitySource, audience string) Option {
	return func(c *Client) {
		if src == nil {
			c.identity = nil
			return
		}
		c.identity = newIdentity(src, audience)
	}
}

func newIdentity(src IdentitySource, audience string) *identity {
	audience = strings.TrimSpace(audience)
	if audience == "" {
		audience = strings.TrimSpace(os.Getenv(EnvSPIFFEAudience))
	}
	if audience == "" {
		audience = DefaultSPIFFEAudience
	}
	return &identity{src: src, audience: audience}
}

func (c *Client) currentIdentity() *identity {
	if c.identity != nil {
		return c.identity
	}
	return defaultIdentity.Load()
}

// jwt returns a JWT-SVID with at least identityMinLife left, or "". It never
// blocks a request for longer than identityFetchTimeout and never returns an
// error: the caller then sends the key alone.
func (i *identity) jwt(ctx context.Context) string {
	now := time.Now()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.token != "" && i.exp.Sub(now) > identityMinLife {
		return i.token
	}
	if now.Before(i.failUntil) {
		// A token that is nearly out of life is still better than none, until it is not.
		if i.token != "" && i.exp.Sub(now) > 10*time.Second {
			return i.token
		}
		return ""
	}
	fctx, cancel := context.WithTimeout(ctx, identityFetchTimeout)
	defer cancel()
	tok, err := i.src.Token(fctx, i.audience)
	exp, ok := jwtExpiry(tok)
	if err != nil || tok == "" || !ok || !exp.After(now.Add(10*time.Second)) {
		i.failUntil = now.Add(identityRetryAfter)
		if i.token != "" && i.exp.Sub(now) > 10*time.Second {
			return i.token
		}
		i.token, i.exp = "", time.Time{}
		return ""
	}
	i.token, i.exp = tok, exp
	return tok
}

// jwtExpiry reads the exp claim of a compact JWT without verifying it (the
// source already did; this only decides when to ask for a new one).
func jwtExpiry(tok string) (time.Time, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}
