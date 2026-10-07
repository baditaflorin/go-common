package middleware

import (
	"encoding/json"
	"net/http"
	"strings"
)

// TokenAuth creates a middleware that validates against a list of allowed
// tokens. Sources checked, in order of precedence:
//
//  1. Authorization: Bearer <token>
//  2. X-API-Key: <token>
//  3. ?api_key=<token> query param (compatibility only; avoid in new clients)
//
// The /health, /version, /selftest, /capabilities, /openapi.json, and /agent.json
// paths bypass auth regardless of token. /capabilities and /openapi.json
// are scraped unauthenticated by the catalog and hub so users can
// discover query flags and the API surface; /agent.json is the agent
// contract — an agent must read it BEFORE it has a key, so it must be
// reachable unauthenticated.
func TokenAuth(validTokens []string) Middleware {
	validMap := make(map[string]bool)
	for _, t := range validTokens {
		if t != "" && !isBlockedSharedCredential(t) {
			validMap[t] = true
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Fleet probes always pass through. Check first so we don't
			// need a token for these no matter how the middleware is
			// wired up.
			if r.URL.Path == "/health" || r.URL.Path == "/version" || r.URL.Path == "/selftest" || r.URL.Path == "/capabilities" || r.URL.Path == "/openapi.json" || r.URL.Path == "/agent.json" {
				next.ServeHTTP(w, r)
				return
			}

			token := ""

			// 1. Authorization: Bearer <token>
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}

			// 2. X-API-Key: <token>
			if token == "" {
				token = r.Header.Get("X-API-Key")
			}

			// 3. ?api_key=<token> query param (compatibility only)
			if token == "" {
				token = r.URL.Query().Get("api_key")
			}

			if token == "" || !validMap[token] {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "Unauthorized",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
