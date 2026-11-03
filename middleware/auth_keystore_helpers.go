package middleware

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/internal/graphidentity"
	"log"
	"net/http"
	"strings"
	"time"
)

// Keep the retired shared credential blocked for old explicit LocalTokens
// configuration without retaining its plaintext value in source.
var blockedSharedCredentialDigest = [32]byte{0x8e, 0xa7, 0x3c, 0xd9, 0xb2, 0x5d, 0xa3, 0x02, 0xdb, 0xd5, 0x43, 0x4b, 0x20, 0xff, 0x79, 0x16, 0x99, 0x64, 0x70, 0xd3, 0x26, 0xdd, 0x40, 0xe7, 0x43, 0xfd, 0x9f, 0x7d, 0xe2, 0xa9, 0x46, 0x2b}

func isBlockedSharedCredential(value string) bool {
	digest := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return subtle.ConstantTimeCompare(digest[:], blockedSharedCredentialDigest[:]) == 1
}

func TokenAuthKeystore(opts KeystoreOpts) Middleware {
	if opts.TrustGatewayHeader == "" {
		opts.TrustGatewayHeader = header.AuthUser
	}
	if opts.VerifyTimeout == 0 {
		opts.VerifyTimeout = 3 * time.Second
	}
	local := make(map[string]bool, len(opts.LocalTokens))
	for _, t := range opts.LocalTokens {
		t = strings.TrimSpace(t)
		if t != "" && !isBlockedSharedCredential(t) {
			local[t] = true
		}
	}
	public := newPublicPrefixes(opts.PublicPathPrefixes)
	logf := func(format string, a ...any) {
		if opts.Logger != nil {
			opts.Logger.Printf(format, a...)
		} else {
			log.Printf(format, a...)
		}
	}

	// X-Auth-* trust switch (XAUTH_TRUST_MODE, auth_xauth.go): off unless the environment says otherwise.
	// In enforce mode this fills opts.TrustedGatewayCIDRs; an explicit list of the service wins.
	xauth := configureXAuth(&opts, logf)
	xauthObs, _ := opts.Observer.(XAuthObserver)

	observe := func(src AuthSource, res AuthResult, d time.Duration) {
		if opts.Observer != nil {
			opts.Observer.ObserveAuth(AuthEvent{Source: src, Result: res, Duration: d})
		}
	}

	idObs, _ := opts.Observer.(IdentityObserver)
	// noteFallback counts a request that an API key authenticated although its
	// principal has a SPIFFE mapping: SPIFFE was expected and not used.
	noteFallback := func(user, reason string) {
		if opts.Identity == nil || idObs == nil || user == "" {
			return
		}
		if principal, ok := opts.Identity.ExpectedPrincipal(user); ok {
			if reason == "" {
				reason = "no_svid"
			}
			idObs.ObserveIdentityFallback(principal, reason)
		}
	}

	// admit is the single funnel every trust path (bypass excluded — see
	// below) routes through before calling next.ServeHTTP. Authentication
	// alone (which token/path/header got the caller this far) is no
	// longer sufficient to dispatch; every path must also state its
	// callerTier so the shared RequiredTier gate applies uniformly.
	//
	// Paths that never verify a real tier (local-token fast path,
	// TrustPrivateMesh) call this with callerTier == "" — which fails
	// TierSatisfies against any non-empty RequiredTier by construction,
	// not because each call site remembered to special-case it.
	admit := func(w http.ResponseWriter, r *http.Request, next http.Handler, src AuthSource, principal, callerTier string, d time.Duration) {
		withCaller := func() *http.Request {
			return r.WithContext(graphidentity.WithVerifiedPrincipal(r.Context(), principal))
		}
		if opts.RequiredTier == "" || apikey.TierSatisfies(callerTier, opts.RequiredTier) {
			observe(src, AuthResultAllow, d)
			next.ServeHTTP(w, withCaller())
			return
		}
		if !opts.TierEnforce {
			// Shadow mode: observe the would-be denial, but don't break
			// traffic yet. See KeystoreOpts.TierEnforce.
			observe(src, AuthResultTierShadowDenied, d)
			next.ServeHTTP(w, withCaller())
			return
		}
		observe(src, AuthResultTierDenied, d)
		denyTier(w, opts.RequiredTier)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. /health, /version, /selftest, /_gw_health, /capabilities always pass —
			//    fleet contract. /capabilities is scraped unauthenticated by
			//    the catalog and hub so users can discover query flags.
			//    A service-declared PublicPathPrefixes entry (opt-in, see its
			//    doc comment) bypasses the same way. Either only for a
			//    canonical path (auth_keystore_path.go): a path that names one
			//    in another form is refused, not authenticated.
			switch classifyPublicPath(r.URL, public) {
			case pathPublic:
				observe(AuthSourceBypass, AuthResultAllow, 0)
				next.ServeHTTP(w, r)
				return
			case pathAmbiguous:
				observe(AuthSourceBypass, AuthResultDeny, 0)
				denyBadPath(w)
				return
			}

			// 1b. SPIFFE identity (dual mode, opt-in via opts.Identity). A valid
			//     SVID or JWT-SVID mapped by the operator authenticates the
			//     caller as that principal. No SVID presented: nothing here,
			//     the checks below run as before. A presented identity that is
			//     not accepted is counted, then either refused (an ID missing
			//     from the map, by default) or left to the API key path.
			bearerIsSVID, idFailReason, idFailSource := false, "", AuthSource("")
			idPresentedFailed := false
			if opts.Identity != nil {
				d := opts.Identity.AuthenticateIdentity(r)
				if d.Presented {
					bearerIsSVID = d.BearerIsSVID
					if d.Allowed {
						// The verified identity replaces anything the caller put in the
						// gateway-owned headers.
						r.Header.Set(header.AuthUser, d.Principal)
						r.Header.Set(header.AuthScope, d.Scope)
						r.Header.Set(header.AuthTier, d.Tier)
						r = r.WithContext(context.WithValue(r.Context(), spiffeIDKey{}, d.SPIFFEID))
						admit(w, r, next, d.Source, d.Principal, d.Tier, 0)
						return
					}
					if idObs != nil {
						idObs.ObserveIdentityFailure(d.Source, d.Reason)
					}
					if d.Deny {
						observe(d.Source, AuthResultDeny, 0)
						denyIdentity(w, d.Status)
						return
					}
					idFailReason, idFailSource = d.Reason, d.Source
					idPresentedFailed = true
				}
			}

			// 2. Gateway already validated? Trust the upstream auth signal only
			//    when it arrived from a configured gateway TCP peer. A direct
			//    caller with spoofed X-Auth-* fields must go through ordinary
			//    key verification and cannot leak its claimed identity downstream.
			//    nginx's auth_request_set captures X-Auth-User from the
			//    keystore's /verify response. Presence ≈ keystore said yes.
			gatewayHeaderPresent := opts.TrustGatewayHeader != "" && r.Header.Get(opts.TrustGatewayHeader) != ""
			if gatewayHeaderPresent && xauth.active() && !trustedGatewayRemoteAddr(r.RemoteAddr, xauth.cidrs) {
				// Shadow and enforce: a request vouched for by the header alone from a peer outside the trusted list.
				xauth.note(r, xauthObs, logf)
			}
			gatewayTrusted := gatewayHeaderPresent && trustedGatewayRemoteAddr(r.RemoteAddr, opts.TrustedGatewayCIDRs)
			if gatewayHeaderPresent && !gatewayTrusted {
				r.Header.Del(header.AuthUser)
				r.Header.Del(header.AuthScope)
				r.Header.Del(header.AuthTier)
			}
			if gatewayTrusted {
				// 2a. Optional defense-in-depth: re-verify the scope
				//     out-of-band so a forged X-Auth-Scope (gateway
				//     compromise or non-gateway path injection) can't
				//     escalate. See KeystoreOpts.OutOfBandScopeCheck.
				if opts.OutOfBandScopeCheck && opts.ScopeChecker != nil {
					claimedScope := r.Header.Get(header.AuthScope)
					token := ExtractToken(r)
					if token == "" {
						// No key to re-verify against. Reject —
						// a request with X-Auth-User but no token
						// could only come from a header forger.
						deny(w, "out-of-band scope check requires a token")
						return
					}
					ctx, cancel := context.WithTimeout(r.Context(), opts.VerifyTimeout)
					err := opts.ScopeChecker.VerifyScope(ctx, token, claimedScope)
					cancel()
					if err != nil {
						if errors.Is(err, apikey.ErrScopeMismatch) {
							logf("scope mismatch: user=%q claimed=%q: %v",
								r.Header.Get(opts.TrustGatewayHeader), claimedScope, err)
							deny(w, "scope mismatch")
							return
						}
						if errors.Is(err, apikey.ErrInvalidKey) {
							deny(w, "invalid token")
							return
						}
						// Keystore unavailable — fail closed (same
						// shape as the primary lookup path below).
						logf("scope check unavailable, denying caller: %v", err)
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("Retry-After", "5")
						w.WriteHeader(http.StatusServiceUnavailable)
						_ = json.NewEncoder(w).Encode(map[string]string{
							"error": "auth backend unavailable; retry shortly",
						})
						return
					}
				}
				// The gateway/nginx path stamps X-Auth-Tier the same way it
				// stamps X-Auth-User/X-Auth-Scope (auth_request_set +
				// proxy_set_header on the auth_request response) — read
				// whatever's there; absent means "" (fails closed against
				// any RequiredTier until that nginx wiring exists).
				// The legacy empty-CIDR mode preserves gateway auth behavior,
				// but does not prove that this request came from the gateway.
				// Only publish a graph identity when the TCP peer was checked
				// against an explicit trusted-gateway allowlist.
				principal := ""
				if len(opts.TrustedGatewayCIDRs) > 0 && trustedGatewayRemoteAddr(r.RemoteAddr, opts.TrustedGatewayCIDRs) {
					principal = r.Header.Get(opts.TrustGatewayHeader)
				}
				noteFallback(r.Header.Get(opts.TrustGatewayHeader), idFailReason)
				admit(w, r, next, AuthSourceGateway, principal, r.Header.Get(header.AuthTier), 0)
				return
			}

			// 2b. Private-mesh trust (opt-in). The request carries no gateway
			//     header (handled above) and its actual TCP peer is a
			//     private/loopback IP — i.e. a sibling container on the docker
			//     mesh, not a public client (which could only arrive via the
			//     gateway path above). Trust it without a token, mirroring the
			//     fetch cache's container-to-container model. Not spoofable:
			//     keyed on r.RemoteAddr, never on a header.
			//     With a strict Identity (StrictIdentity), a request that presented a
			//     SPIFFE identity that was not accepted is not admitted here: it needs
			//     a key (step 3) or is refused.
			if opts.TrustPrivateMesh && !(idPresentedFailed && identityStrict(opts.Identity)) && isPrivateRemoteAddr(r.RemoteAddr) {
				// callerTier "" by design — a container-to-container call
				// never verified a tier, so it can't satisfy a tier gate.
				// If a tiered private-mesh caller is ever needed, that's a
				// deliberate future option (e.g. a MeshTier field), not an
				// accidental bypass of this one.
				admit(w, r, next, AuthSourcePrivateMesh, "", "", 0)
				return
			}

			// 3. Extract the raw token from the supported sources: Bearer,
			//    X-API-Key, or the compatibility query parameter.
			token := ExtractToken(r)
			if bearerIsSVID {
				token = apiKeyWithoutBearer(r)
			}
			if token == "" && idFailReason != "" {
				// A SPIFFE identity was offered, was not accepted, and no API key
				// came with it: say so, not "missing token".
				observe(idFailSource, AuthResultDeny, 0)
				denyIdentity(w, http.StatusUnauthorized)
				return
			}
			if token == "" {
				observe(AuthSourceMissing, AuthResultDeny, 0)
				deny(w, "missing token")
				return
			}

			// 4. Explicit service-owned local credential fast path. Avoids a
			//    network hop only for credentials configured by this service.
			if local[token] {
				// callerTier "" by design — a local credential never
				// touches the keystore, so it never carries a real tier.
				// It cannot satisfy a non-empty RequiredTier; there is no
				// "the demo key is secretly vetted-pentest" escape hatch.
				admit(w, r, next, AuthSourceLocal, "", "", 0)
				return
			}

			// 5. Keystore lookup with timeout. Verifier is typically an
			//    apikey.Cache so transient keystore outages serve stale-
			//    but-valid results for up to its StaleTTL.
			ctx, cancel := context.WithTimeout(r.Context(), opts.VerifyTimeout)
			defer cancel()
			start := time.Now()
			res, err := opts.Verifier.Verify(ctx, token)
			dur := time.Since(start)
			if err == nil {
				// Surface user + scope to downstream handlers if anyone
				// wants them. Headers are clobbered (not appended) so
				// callers can't smuggle their own.
				r.Header.Set(header.AuthUser, res.User)
				r.Header.Set(header.AuthScope, res.Scope)
				r.Header.Set(header.AuthTier, res.Tier)
				noteFallback(res.User, idFailReason)
				admit(w, r, next, AuthSourceKeystore, res.User, res.Tier, dur)
				return
			}
			if errors.Is(err, apikey.ErrInvalidKey) {
				observe(AuthSourceKeystore, AuthResultDeny, dur)
				deny(w, "invalid token")
				return
			}
			// Keystore unavailable AND no cached result. Fail closed —
			// better a 503 than a free-for-all if the keystore is offline
			// and the caller isn't on the local-tokens list.
			observe(AuthSourceKeystore, AuthResultUnavailable, dur)
			logf("keystore unavailable, denying caller: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "auth backend unavailable; retry shortly",
			})
		})
	}
}
