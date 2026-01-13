package middleware

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter manages rate limits per visitor
type RateLimiter struct {
	visitors map[string]*rate.Limiter
	mu       sync.Mutex
	r        rate.Limit
	b        int
}

// NewRateLimiter creates a new rate limiter
// r: limit (events per second)
// b: burst size
func NewRateLimiter(r rate.Limit, b int) *RateLimiter {
	rl := &RateLimiter{
		visitors: make(map[string]*rate.Limiter),
		r:        r,
		b:        b,
	}

	// Cleanup routine
	go rl.cleanup()

	return rl
}

func (rl *RateLimiter) getLimiter(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, exists := rl.visitors[ip]
	if !exists {
		limiter = rate.NewLimiter(rl.r, rl.b)
		rl.visitors[ip] = limiter
	}

	return limiter
}

func (rl *RateLimiter) cleanup() {
	for {
		time.Sleep(time.Minute)
		rl.mu.Lock()
		// In a real implementation, we'd track last seen time and delete old entries
		// For simplicity/MVP, we just clear for now or simple "reset"
		// A better approach involves a struct wrapper with LastSeen
		// keeping it simple for now to avoid complexity overhead
		rl.visitors = make(map[string]*rate.Limiter)
		rl.mu.Unlock()
	}
}

// RateLimit creates a middleware that enforces rate limits
func RateLimit(limit int, burst int) Middleware {
	rl := NewRateLimiter(rate.Limit(limit), burst)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := r.RemoteAddr // Simplified, should parse headers if behind proxy
			// In our infra, Nginx sets X-Real-IP or similar, but RemoteAddr is decent default

			limiter := rl.getLimiter(ip)
			if !limiter.Allow() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(map[string]string{
					"error": "Too Many Requests",
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
