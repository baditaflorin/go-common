package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/header"
)

// TokenAuthKeystore is the canonical fleet auth middleware once a service
// has been migrated to keystore-backed validation.
//
//   - Trust the gateway. Every request that reaches the service through
//     nginx has already been validated by the gateway's auth_request →
//     keystore /verify chain. We don't second-guess.
//   - Local fallback only. If the gateway is somehow bypassed (direct
//     container access during dev, internal mesh callers without nginx),
//     fall back to the same static-list check as the legacy TokenAuth so
//     existing behavior doesn't regress.
//
// Migration path for a service:
//
//   - r := mux.NewRouter()
//
//   - import "github.com/baditaflorin/go-common/middleware"
//
//   - import "github.com/baditaflorin/go-common/apikey"
//
//     ks := apikey.NewCache(apikey.New())
//
//   - r.Use(middleware.TokenAuth([]string{os.Getenv("API_KEYS")…}))
//
//   - r.Use(middleware.TokenAuthKeystore(middleware.KeystoreOpts{
//
//   - Verifier:    ks,
//
//   - LocalTokens: strings.Split(os.Getenv("API_KEYS"), ","),
//
//   - }))
//
// One-line library change → every service that bumps go-common picks up
// keystore auth. No per-repo handler rewrite.
type KeystoreOpts struct {
	// Verifier is the keystore client (or its Cache wrapper). Required.
	Verifier apikey.Verifier

	// LocalTokens are accepted without hitting the keystore — fast path for
	// the gateway's static-fallback key (`fb_05dea…`) and the legacy
	// `default_token`. Empty = no local fallback.
	LocalTokens []string

	// TrustGatewayHeader: if non-empty, requests carrying this header are
	// treated as already-authenticated (the gateway sets X-Auth-User after
	// the keystore returned 200). Skip both keystore and local check.
	// Default header.AuthUser.
	TrustGatewayHeader string

	// TrustPrivateMesh, when true, treats a request whose actual TCP peer
	// (r.RemoteAddr — NOT a spoofable header) is a private/loopback IP AND
	// that carries no gateway trust header as already-authenticated. This is
	// the "container-to-container on the docker mesh" trust the fleet fetch
	// cache relies on, made reusable: an expensive internal-only service
	// (e.g. the chromedp js-proxy) can be reached no-auth by sibling
	// containers while its PUBLIC gateway URL stays fully keystore-gated.
	//
	// Safe because: public clients can only reach the container via nginx,
	// which connects from the gateway and sets the trust header (so they take
	// the gateway path, not this one); a public client cannot present a
	// private source IP on the container's network. Default false — no effect
	// on any service that doesn't opt in.
	TrustPrivateMesh bool

	// VerifyTimeout caps the upstream keystore call. Default 3s.
	VerifyTimeout time.Duration

	// Logger receives one-line audit lines for accepted/rejected requests.
	// nil = use the default package log. Pass a no-op to silence.
	Logger *log.Logger

	// Observer (optional) receives one AuthEvent per request describing
	// which code path made the decision and how long the verifier call
	// took. promx.NewAuthCollectors() returns an implementation that
	// records fleet-canonical Prometheus metrics.
	Observer AuthObserver

	// OutOfBandScopeCheck enables defense-in-depth verification that the
	// gateway-supplied X-Auth-Scope matches what the keystore actually
	// reports for the principal. Opt-in (default false) because it costs
	// one extra keystore call per (key, scope) per 5 min per service.
	//
	// Threat model: if the gateway were ever compromised (or a non-gateway
	// caller forged X-Auth-* headers and bypassed nginx via the docker
	// mesh), a service trusting only the gateway header would honor a
	// forged scope. With this on, every request whose scope is consumed
	// for an authorization decision is independently re-verified via
	// apikey.Client.VerifyScope, which calls /verify and compares the
	// keystore's authoritative scope to the claimed one.
	//
	// On mismatch the request is rejected 401. On keystore outage the
	// request follows the same fail-closed path as the primary keystore
	// check (503).
	//
	// The check only runs on the gateway-header trust path (step 2) —
	// the keystore-lookup path (step 5) already sets X-Auth-Scope from
	// the same authoritative response and is not vulnerable.
	//
	// Requires ScopeChecker to be set (typically the underlying
	// *apikey.Client) AND the request to carry a usable token (Bearer /
	// X-API-Key / ?api_key). If only the gateway header is set with no
	// key, the check cannot run and the request is rejected.
	OutOfBandScopeCheck bool

	// ScopeChecker performs the out-of-band re-verification. Required
	// when OutOfBandScopeCheck is true. Typically the underlying
	// *apikey.Client (the *apikey.Cache wrapper used as Verifier does
	// not expose VerifyScope — keep a reference to the raw client).
	ScopeChecker ScopeChecker
}

// ScopeChecker is the abstract interface for out-of-band scope
// verification. *apikey.Client satisfies it via its VerifyScope method.
type ScopeChecker interface {
	VerifyScope(ctx context.Context, key, claimedScope string) error
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

// extractToken pulls the API key from the three canonical sources, in
// priority order:
//
//  1. Authorization: Bearer <key>     — what every SDK and API gateway sends
//  2. X-API-Key: <key>                — legacy header alias
//  3. ?api_key=<key>                  — demo / browser-playground only
//
// The legacy /t/<token>/ path-prefix extraction was removed in
// go-common v0.11.0 (2026-05-14). Gateway returns 410 Gone for that
// shape, so any caller still using it is broken at the edge anyway —
// no need to honor it at the upstream. Defense in depth.
func extractToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return strings.TrimPrefix(v, "Bearer ")
	}
	if v := r.Header.Get(header.APIKey); v != "" {
		return v
	}
	return r.URL.Query().Get("api_key")
}

func deny(w http.ResponseWriter, why string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized", "reason": why})
}

// isPrivateRemoteAddr reports whether addr (an http.Request.RemoteAddr in
// "host:port" form) is a loopback, private (RFC1918 / ULA), or link-local
// IP — i.e. a peer on the docker/private mesh rather than a public client.
// Used only by the opt-in TrustPrivateMesh path. A malformed or public
// address returns false (fail closed). Keyed on the real TCP peer, never a
// header, so it cannot be spoofed by a request claiming a private origin.
func isPrivateRemoteAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
