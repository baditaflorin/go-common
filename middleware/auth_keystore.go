package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/apikey"
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
//   + import "github.com/baditaflorin/go-common/middleware"
//   + import "github.com/baditaflorin/go-common/apikey"
//
//     ks := apikey.NewCache(apikey.New())
//   - r.Use(middleware.TokenAuth([]string{os.Getenv("API_KEYS")…}))
//   + r.Use(middleware.TokenAuthKeystore(middleware.KeystoreOpts{
//   +     Verifier:    ks,
//   +     LocalTokens: strings.Split(os.Getenv("API_KEYS"), ","),
//   + }))
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
	// Default "X-Auth-User".
	TrustGatewayHeader string

	// VerifyTimeout caps the upstream keystore call. Default 3s.
	VerifyTimeout time.Duration

	// Logger receives one-line audit lines for accepted/rejected requests.
	// nil = use the default package log. Pass a no-op to silence.
	Logger *log.Logger
}

func TokenAuthKeystore(opts KeystoreOpts) Middleware {
	if opts.TrustGatewayHeader == "" {
		opts.TrustGatewayHeader = "X-Auth-User"
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

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. /health and /version always pass — fleet contract.
			if r.URL.Path == "/health" || r.URL.Path == "/version" || r.URL.Path == "/_gw_health" {
				next.ServeHTTP(w, r)
				return
			}

			// 2. Gateway already validated? Trust the upstream auth signal.
			//    nginx's auth_request_set captures X-Auth-User from the
			//    keystore's /verify response. Presence ≈ keystore said yes.
			if opts.TrustGatewayHeader != "" && r.Header.Get(opts.TrustGatewayHeader) != "" {
				next.ServeHTTP(w, r)
				return
			}

			// 3. Extract the raw token from the same three sources legacy
			//    TokenAuth checks: Bearer header, /t/<token>/ path, ?api_key=.
			token := extractToken(r)
			if token == "" {
				deny(w, "missing token")
				return
			}

			// 4. Local-token fast path (the gateway's static fallback, demo
			//    token, etc.). Avoids a network hop for the hot common case.
			if local[token] {
				next.ServeHTTP(w, r)
				return
			}

			// 5. Keystore lookup with timeout. Verifier is typically an
			//    apikey.Cache so transient keystore outages serve stale-
			//    but-valid results for up to its StaleTTL.
			ctx, cancel := context.WithTimeout(r.Context(), opts.VerifyTimeout)
			defer cancel()
			res, err := opts.Verifier.Verify(ctx, token)
			if err == nil {
				// Surface user + scope to downstream handlers if anyone
				// wants them. Headers are clobbered (not appended) so
				// callers can't smuggle their own.
				r.Header.Set("X-Auth-User", res.User)
				r.Header.Set("X-Auth-Scope", res.Scope)
				next.ServeHTTP(w, r)
				return
			}
			if errors.Is(err, apikey.ErrInvalidKey) {
				deny(w, "invalid token")
				return
			}
			// Keystore unavailable AND no cached result. Fail closed —
			// better a 503 than a free-for-all if the keystore is offline
			// and the caller isn't on the local-tokens list.
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

func extractToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return strings.TrimPrefix(v, "Bearer ")
	}
	if v := r.Header.Get("X-API-Key"); v != "" {
		return v
	}
	for i, p := range strings.Split(r.URL.Path, "/") {
		if p == "t" {
			parts := strings.Split(r.URL.Path, "/")
			if i+1 < len(parts) {
				return parts[i+1]
			}
		}
	}
	return r.URL.Query().Get("api_key")
}

func deny(w http.ResponseWriter, why string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized", "reason": why})
}
