package middleware

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/baditaflorin/go-common/header"
)

// The SPIFFE identity path of TokenAuthKeystore.
//
// KeystoreOpts.Identity lets a request authenticate with a SPIFFE identity (an
// X.509-SVID presented over mTLS, or a JWT-SVID) instead of an API key. It is
// DUAL MODE: a request that carries no SPIFFE identity goes through the
// existing checks exactly as before, so nothing is rejected for lacking one.
// The verification (go-spiffe, the Workload API) lives in package spiffeauth,
// which implements IdentityAuthenticator; middleware only defines the contract
// so that it keeps its dependency budget (scripts/dep-budget.txt).

const (
	// AuthSourceSPIFFEMTLS: authenticated by an X.509-SVID presented over mTLS
	// and mapped to a principal (package spiffeauth).
	AuthSourceSPIFFEMTLS AuthSource = "spiffe-mtls"
	// AuthSourceSPIFFEJWT: authenticated by an audience-scoped JWT-SVID.
	AuthSourceSPIFFEJWT AuthSource = "spiffe-jwt"
)

// IdentityDecision is what an IdentityAuthenticator concludes about one request.
type IdentityDecision struct {
	// Presented: the caller offered a SPIFFE identity (an SVID certificate or a
	// JWT-SVID). false means nothing in this request concerns the SPIFFE path.
	Presented bool
	// Allowed: the identity verified and maps to a principal. Principal, Scope,
	// Tier and SPIFFEID are set.
	Allowed bool
	// Source is AuthSourceSPIFFEMTLS or AuthSourceSPIFFEJWT (when Presented).
	Source AuthSource
	// SPIFFEID is the verified SPIFFE ID (only when Allowed).
	SPIFFEID string
	// Principal, Scope and Tier come from the operator's peer map.
	Principal, Scope, Tier string
	// Reason is a short, bounded token naming why a presented identity was not
	// allowed (for the failure counter). Never request data.
	Reason string
	// Deny: refuse the request now with Status (401 or 403) instead of trying
	// the API key path (an SVID of an ID that is not in the map, by default).
	Deny   bool
	Status int
	// BearerIsSVID: the Authorization bearer value was a JWT-SVID, so the API key
	// path must not read it as an API key (and must not send it to the keystore).
	BearerIsSVID bool
}

// IdentityAuthenticator verifies the SPIFFE identity of a request. Implemented
// by spiffeauth.Authenticator. Methods run on the request hot path and must not
// block on the network.
type IdentityAuthenticator interface {
	AuthenticateIdentity(r *http.Request) IdentityDecision
	// ExpectedPrincipal reports whether keystoreUser is a principal that has a
	// SPIFFE mapping (so a request that authenticated it with an API key is a
	// fallback), and returns the operator-configured principal string.
	ExpectedPrincipal(keystoreUser string) (string, bool)
}

// IdentityObserver is optional: an AuthObserver that also implements it
// receives the SPIFFE failure and fallback events. promx.AuthCollectors does.
type IdentityObserver interface {
	// ObserveIdentityFailure: a presented SPIFFE identity was not accepted.
	// reason is one of the spiffeauth.Reason* tokens.
	ObserveIdentityFailure(source AuthSource, reason string)
	// ObserveIdentityFallback: SPIFFE was expected for principal (it has a
	// mapping) but the request was authenticated with an API key. reason is
	// "no_svid" or the reason the presented identity failed.
	ObserveIdentityFallback(principal, reason string)
}

type spiffeIDKey struct{}

// SPIFFEIDFromContext returns the verified SPIFFE ID of a request that
// TokenAuthKeystore authenticated through the SPIFFE path, or "".
func SPIFFEIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(spiffeIDKey{}).(string)
	return id
}

// denyIdentity refuses a request whose SPIFFE identity was presented but is not
// accepted. The body names no detail beyond the generic reason.
func denyIdentity(w http.ResponseWriter, status int) {
	if status != http.StatusForbidden {
		status = http.StatusUnauthorized
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	text := "Unauthorized"
	why := "invalid SPIFFE identity"
	if status == http.StatusForbidden {
		text, why = "Forbidden", "SPIFFE peer not permitted"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"error": text, "reason": why})
}

// apiKeyWithoutBearer is ExtractToken minus the Authorization bearer: the
// token a request carries as an API key when its bearer is a JWT-SVID.
func apiKeyWithoutBearer(r *http.Request) string {
	if v := r.Header.Get(header.APIKey); v != "" {
		return v
	}
	return r.URL.Query().Get("api_key")
}
