package server

import (
	"github.com/baditaflorin/go-common/middleware"
)

// WithIdempotencyKey mounts the canonical idempotency-key middleware
// on the server. Clients retrying a mutating request (POST/PUT/PATCH/
// DELETE by default) with the same Idempotency-Key header receive the
// cached response instead of re-executing the handler.
//
// Typical use:
//
//	srv := server.New(cfg,
//	    server.WithKeystoreAuth("default_token"),
//	    server.WithIdempotencyKey(middleware.IdempotencyConfig{
//	        TTL:        15 * time.Minute,
//	        MaxEntries: 50_000,
//	    }),
//	)
//
// Pass middleware.IdempotencyConfig{} to accept defaults (1h TTL,
// 10k entry LRU, POST/PUT/PATCH/DELETE methods). See the middleware
// package for the full contract and caveats — notably, the cache is
// process-local; multi-replica services need an external store.
func WithIdempotencyKey(cfg middleware.IdempotencyConfig) Option {
	return func(s *Server) {
		s.Middlewares = append(s.Middlewares, middleware.IdempotencyKey(cfg))
	}
}
