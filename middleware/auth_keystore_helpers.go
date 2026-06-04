package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/header"
	"log"
	"net/http"
	"strings"
	"time"
)

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
		if t != "" {
			local[t] = true
		}
	}
	logf := func(format string, a ...any) {
		if opts.Logger != nil {
			opts.Logger.Printf(format, a...)
		} else {
			log.Printf(format, a...)
		}
	}

	observe := func(src AuthSource, res AuthResult, d time.Duration) {
		if opts.Observer != nil {
			opts.Observer.ObserveAuth(AuthEvent{Source: src, Result: res, Duration: d})
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. /health, /version, /_gw_health, /capabilities always pass —
			//    fleet contract. /capabilities is scraped unauthenticated by
			//    the catalog and hub so users can discover query flags.
			if r.URL.Path == "/health" || r.URL.Path == "/version" || r.URL.Path == "/_gw_health" || r.URL.Path == "/capabilities" || r.URL.Path == "/openapi.json" {
				observe(AuthSourceBypass, AuthResultAllow, 0)
				next.ServeHTTP(w, r)
				return
			}

			// 2. Gateway already validated? Trust the upstream auth signal.
			//    nginx's auth_request_set captures X-Auth-User from the
			//    keystore's /verify response. Presence ≈ keystore said yes.
			if opts.TrustGatewayHeader != "" && r.Header.Get(opts.TrustGatewayHeader) != "" {
				// 2a. Optional defense-in-depth: re-verify the scope
				//     out-of-band so a forged X-Auth-Scope (gateway
				//     compromise or non-gateway path injection) can't
				//     escalate. See KeystoreOpts.OutOfBandScopeCheck.
				if opts.OutOfBandScopeCheck && opts.ScopeChecker != nil {
					claimedScope := r.Header.Get(header.AuthScope)
					token := extractToken(r)
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
				observe(AuthSourceGateway, AuthResultAllow, 0)
				next.ServeHTTP(w, r)
				return
			}

			// 2b. Private-mesh trust (opt-in). The request carries no gateway
			//     header (handled above) and its actual TCP peer is a
			//     private/loopback IP — i.e. a sibling container on the docker
			//     mesh, not a public client (which could only arrive via the
			//     gateway path above). Trust it without a token, mirroring the
			//     fetch cache's container-to-container model. Not spoofable:
			//     keyed on r.RemoteAddr, never on a header.
			if opts.TrustPrivateMesh && isPrivateRemoteAddr(r.RemoteAddr) {
				observe(AuthSourcePrivateMesh, AuthResultAllow, 0)
				next.ServeHTTP(w, r)
				return
			}

			// 3. Extract the raw token from the same three sources legacy
			//    TokenAuth checks: Bearer header, /t/<token>/ path, ?api_key=.
			token := extractToken(r)
			if token == "" {
				observe(AuthSourceMissing, AuthResultDeny, 0)
				deny(w, "missing token")
				return
			}

			// 4. Local-token fast path (the gateway's static fallback, demo
			//    token, etc.). Avoids a network hop for the hot common case.
			if local[token] {
				observe(AuthSourceLocal, AuthResultAllow, 0)
				next.ServeHTTP(w, r)
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
				observe(AuthSourceKeystore, AuthResultAllow, dur)
				next.ServeHTTP(w, r)
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
