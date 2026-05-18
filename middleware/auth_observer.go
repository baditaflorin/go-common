package middleware

import "time"

// AuthObserver receives one event per request that passes through
// TokenAuthKeystore. Implementations MUST NOT block — callbacks run inline
// on the request hot path. The canonical implementation lives in
// go-common/promx and records Prometheus counters/histograms.
//
// middleware deliberately defines the contract here rather than importing
// a metrics library directly: go-common/middleware keeps zero metric-stack
// deps, services pull in promx (and its client_golang transitive set)
// only when they want fleet metrics.
type AuthObserver interface {
	ObserveAuth(AuthEvent)
}

// AuthEvent is the per-request payload handed to an AuthObserver.
// Source records which code path made the decision (so operators can
// see, e.g., what fraction of traffic is already gateway-authenticated
// vs. hitting the keystore). Result records the outcome.
type AuthEvent struct {
	Source   AuthSource
	Result   AuthResult
	Duration time.Duration
}

// AuthSource buckets the path that produced the auth decision.
type AuthSource string

const (
	AuthSourceBypass   AuthSource = "bypass"   // exempt path: /health, /version, /_gw_health, /capabilities
	AuthSourceGateway  AuthSource = "gateway"  // upstream nginx set the trust header
	AuthSourceLocal    AuthSource = "local"    // local-token fast path
	AuthSourceKeystore AuthSource = "keystore" // upstream keystore call (possibly cached)
	AuthSourceMissing  AuthSource = "missing"  // no token presented
)

// AuthResult buckets the outcome.
type AuthResult string

const (
	AuthResultAllow       AuthResult = "allow"
	AuthResultDeny        AuthResult = "deny"        // 401: invalid or missing token
	AuthResultUnavailable AuthResult = "unavailable" // 503: keystore down + no cache
)
