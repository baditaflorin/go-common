// Package middleware provides composable HTTP middleware for fleet services.
//
// Canonical middleware included:
//   - RequestID   — generates X-Request-ID; injects into context
//   - Logging     — slog JSON per-request log; injects child logger into ctx
//   - Metrics     — per-request counters for /metrics/json
//   - CORS        — canonical CORS headers with safe defaults
//   - TokenAuth   — static-token Bearer validation
//   - TokenAuthKeystore — fleet-canonical keystore auth
//   - Chain       — compose multiple middlewares left=outermost
//
// Logger retrieval in handlers:
//
//	log := middleware.LoggerFromContext(r.Context())
//	log.Info("processing", "user", userID)
package middleware
