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
//  2. URL path /t/{token}/...     (legacy support)
//  3. ?api_key=<token> query param (browser-friendly)
//
// The /health and /version paths bypass auth regardless of token.
func TokenAuth(validTokens []string) Middleware {
	validMap := make(map[string]bool)
	for _, t := range validTokens {
		validMap[t] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health/version always pass through. Check first so we don't
			// need a token for these no matter how the middleware is
			// wired up.
			if r.URL.Path == "/health" || r.URL.Path == "/version" {
				next.ServeHTTP(w, r)
				return
			}

			token := ""

			// 1. Authorization: Bearer <token>
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}

			// 2. Legacy /t/{token}/ path
			if token == "" {
				parts := strings.Split(r.URL.Path, "/")
				for i, p := range parts {
					if p == "t" && i+1 < len(parts) {
						token = parts[i+1]
						break
					}
				}
			}

			// 3. ?api_key=<token> query param (browser-friendly)
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
