package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/baditaflorin/go-common/header"
)

// CORSOptions configures the CORS middleware. The zero value is safe
// and denies all cross-origin requests (same as no CORS headers).
type CORSOptions struct {
	// AllowedOrigins is the list of origins that are allowed. Use "*"
	// to allow all origins (not recommended for credentialed requests).
	// An empty list disables CORS (no Access-Control-Allow-Origin header).
	AllowedOrigins []string

	// AllowedMethods lists the HTTP methods allowed for CORS requests.
	// Default (when nil): GET, POST, PUT, PATCH, DELETE, OPTIONS.
	AllowedMethods []string

	// AllowedHeaders lists the request headers allowed in CORS requests.
	// Default (when nil): Content-Type, Authorization, X-API-Key,
	// X-Request-ID.
	AllowedHeaders []string

	// ExposedHeaders lists response headers accessible to the browser.
	ExposedHeaders []string

	// AllowCredentials sets Access-Control-Allow-Credentials: true.
	// Must NOT be combined with AllowedOrigins=["*"] — browsers block that.
	AllowCredentials bool

	// MaxAgeSecs sets Access-Control-Max-Age (preflight cache duration).
	// Default 3600 (1 hour). Set to -1 to omit the header.
	MaxAgeSecs int
}

var defaultMethods = []string{
	http.MethodGet, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

var defaultHeaders = []string{
	header.ContentType,
	header.Authorization,
	header.APIKey,
	header.RequestID,
}

// CORS returns an http.Handler middleware that sets canonical CORS
// response headers according to opts. It handles OPTIONS preflight
// requests by returning 204 No Content immediately, and sets
// CORS headers on all other responses.
//
// Secure defaults:
//   - AllowedOrigins must be explicit; "*" is accepted but logged.
//   - AllowCredentials + wildcard origin is rejected with a panic.
//   - MaxAgeSecs defaults to 3600.
//
// Usage:
//
//	server.New(cfg, server.WithMiddleware(
//	    middleware.CORS(middleware.CORSOptions{
//	        AllowedOrigins: []string{"https://app.example.com"},
//	    }),
//	))
func CORS(opts CORSOptions) Middleware {
	// Validate: AllowCredentials + wildcard is a security mistake.
	for _, o := range opts.AllowedOrigins {
		if o == "*" && opts.AllowCredentials {
			panic("middleware.CORS: AllowCredentials=true with AllowedOrigins=[*] is unsafe; use explicit origins")
		}
	}
	methods := opts.AllowedMethods
	if len(methods) == 0 {
		methods = defaultMethods
	}
	headers := opts.AllowedHeaders
	if len(headers) == 0 {
		headers = defaultHeaders
	}
	maxAge := opts.MaxAgeSecs
	if maxAge == 0 {
		maxAge = 3600
	}

	methodsStr := strings.Join(methods, ", ")
	headersStr := strings.Join(headers, ", ")
	var exposedStr string
	if len(opts.ExposedHeaders) > 0 {
		exposedStr = strings.Join(opts.ExposedHeaders, ", ")
	}

	originSet := make(map[string]struct{}, len(opts.AllowedOrigins))
	allowAll := false
	for _, o := range opts.AllowedOrigins {
		if o == "*" {
			allowAll = true
		}
		originSet[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get(header.Origin)

			// No Origin header: not a CORS request; pass through.
			if origin == "" || len(opts.AllowedOrigins) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			// Decide whether to echo the Origin.
			allowed := allowAll
			if !allowed {
				_, allowed = originSet[origin]
			}

			if allowed {
				if allowAll {
					w.Header().Set(header.AccessControlAllowOrigin, "*")
				} else {
					w.Header().Set(header.AccessControlAllowOrigin, origin)
					// Vary: Origin tells caches not to serve this response to other origins.
					w.Header().Add("Vary", header.Origin)
				}
				w.Header().Set(header.AccessControlAllowMethods, methodsStr)
				w.Header().Set(header.AccessControlAllowHeaders, headersStr)
				if exposedStr != "" {
					w.Header().Set(header.AccessControlExposeHeaders, exposedStr)
				}
				if opts.AllowCredentials {
					w.Header().Set(header.AccessControlAllowCredentials, "true")
				}
				if maxAge > 0 {
					w.Header().Set(header.AccessControlMaxAge, strconv.Itoa(maxAge))
				}
			}

			// Handle preflight.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
