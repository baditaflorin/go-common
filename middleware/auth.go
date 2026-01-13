package middleware

import (
	"encoding/json"
	"net/http"
	"strings"
)

// TokenAuthMiddleware creates a middleware that validates against a list of allowed tokens.
// It checks:
// 1. Authorization Header (Bearer token) (Prioritized)
// 2. URL Path (e.g. /t/TOKEN/...) (Legacy support)
func TokenAuth(validTokens []string) Middleware {
	validMap := make(map[string]bool)
	for _, t := range validTokens {
		validMap[t] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := ""

			// 1. Check Header
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}

			// 2. Check Path (Legacy /t/{token}/ pattern)
			// This logic is specific to our /t/{token}/ structure
			if token == "" {
				parts := strings.Split(r.URL.Path, "/")
				for i, p := range parts {
					if p == "t" && i+1 < len(parts) {
						token = parts[i+1]
						break
					}
				}
			}

			// If no token found or invalid
			if token == "" || !validMap[token] {
				// Special case: /health and /version should bypass auth?
				// Usually yes, but middleware placement decides that.
				// If we put this middleware GLOBAL, we must skip specific paths.
				if r.URL.Path == "/health" || r.URL.Path == "/version" {
					next.ServeHTTP(w, r)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "Unauthorized",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
