package safehttp

import "time"

// EgressObserver receives one event per completed outbound HTTP attempt
// (success, error, or timeout). Implementations MUST NOT block — observer
// callbacks run inline on the request hot path. The canonical implementation
// lives in go-common/promx and records Prometheus counters/histograms.
//
// safehttp deliberately defines the contract here rather than importing a
// metrics library directly: go-common/safehttp keeps zero metric-stack deps,
// services pull in promx (and its prometheus/client_golang transitive set)
// only when they want fleet metrics.
type EgressObserver interface {
	ObserveEgress(EgressEvent)
}

// EgressEvent is the per-request payload handed to an EgressObserver. All
// fields are populated whether the request succeeded or failed; Err is non-
// nil when the round-trip itself failed (DNS, dial, TLS, timeout, SSRF
// block). HTTP-level non-2xx responses are NOT errors — inspect Status.
//
// Outcome is a coarse bucket safe to use as a Prometheus label (bounded
// cardinality). Host is the request URL's hostname (no port). Bytes is the
// number of response body bytes observed if known, else 0.
type EgressEvent struct {
	Method     string
	Host       string
	Scheme     string
	Path       string
	Status     int           // 0 if Err != nil
	Duration   time.Duration
	Bytes      int64         // response body bytes; 0 if unknown
	ViaProxy   bool          // true if the request was sent through an HTTP(S)_PROXY
	ProxyHost  string        // host of the proxy used, "" if direct
	Outcome    EgressOutcome // bucketed for label cardinality safety
	Err        error         // nil on HTTP-level responses (even 4xx/5xx)
}

// EgressOutcome buckets request results into a small, label-safe set.
type EgressOutcome string

const (
	OutcomeSuccess     EgressOutcome = "success"      // 2xx
	OutcomeRedirect    EgressOutcome = "redirect"     // 3xx (post-CheckRedirect)
	OutcomeClientError EgressOutcome = "client_error" // 4xx
	OutcomeServerError EgressOutcome = "server_error" // 5xx
	OutcomeBlocked     EgressOutcome = "blocked"      // SSRF guard rejected the dial
	OutcomeDNSFail     EgressOutcome = "dns_fail"     // resolver failure
	OutcomeTimeout     EgressOutcome = "timeout"      // context deadline / Transport timeout
	OutcomeTLSFail     EgressOutcome = "tls_fail"     // handshake failure not covered by the 1.2 fallback
	OutcomeNetError    EgressOutcome = "net_error"    // dial reset / EOF / generic transport error
)

// WithObserver attaches an EgressObserver to the client. The observer
// receives one EgressEvent per round-trip attempt, inline on the hot path
// (so the implementation must be cheap and non-blocking). Use promx
// .NewEgressCollectors(...) in a service to get fleet-canonical metrics
// without writing a custom observer.
func WithObserver(o EgressObserver) Option {
	return func(opts *options) { opts.observer = o }
}
