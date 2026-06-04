package selftest

// Category classifies a check by its probe semantics, matching
// Kubernetes and fleet-runner conventions.
//
//	CategoryLiveness   — is the process alive and not deadlocked?
//	                     Failing = container restart.
//	CategoryReadiness  — is the service ready to accept traffic?
//	                     Failing = removed from load balancer.
//	CategoryStartup    — did the service initialise correctly?
//	                     Failing = prevents readiness from being checked.
//	CategoryAny        — matches all categories (used by ?category= filter).
type Category string

const (
	// CategoryLiveness checks whether the process is alive and not hung.
	CategoryLiveness Category = "liveness"
	// CategoryReadiness checks whether the service can serve traffic.
	CategoryReadiness Category = "readiness"
	// CategoryStartup checks whether one-time initialisation completed.
	CategoryStartup Category = "startup"
	// CategoryAny is a wildcard that matches all checks. Returned when no
	// ?category= query parameter is provided.
	CategoryAny Category = ""
)
