// Package resilience composes the fleet's outbound-call protections into one
// policy: a load-shed gate, a circuit breaker, and retries limited by a budget.
//
// The pieces already exist (loadshed, circuitbreaker, retry). Wired by hand they
// are usually wired badly: retries multiply load exactly when an upstream is
// struggling, and a breaker that retries never see opens. A Policy fixes the
// order and, above all, puts a retry budget in front of every retry.
//
//	p := resilience.New("ledger",
//	    resilience.WithBreaker(circuitbreaker.New(circuitbreaker.Options{Upstream: "ledger"})),
//	    resilience.WithShed(loadshed.New("ledger", 64)),
//	)
//	err := p.Do(ctx, func(ctx context.Context) error { return call(ctx) })
//
// Each call goes through, in order:
//
//  1. the shed gate: over the concurrency limit the call is refused at once
//     (ErrShed), without touching the upstream;
//  2. the breaker: while it is open the call is refused (ErrCircuitOpen);
//  3. the attempt, then if it fails with an error Retryable reports as
//     retryable, a retry after a backoff, but only if the retry budget has a
//     token. The budget gets a fraction of a token per call and a small
//     steady refill, so retries stay a small share of traffic however badly the
//     upstream is failing, while a quiet service can still retry a blip.
//
// Only failures that look like upstream trouble (Retryable) count against the
// breaker; a 404 or a validation error says the upstream is healthy.
//
// A rejection (ErrShed, ErrCircuitOpen) is never retried and is not Retryable:
// the caller should fail open or fail closed, not hammer the same path.
package resilience
