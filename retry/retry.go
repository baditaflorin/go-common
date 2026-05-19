// Package retry provides context-aware retry logic with configurable
// backoff strategies. It replaces ad-hoc for-loops scattered across
// fleet services with one correct implementation of exponential
// backoff + full jitter.
//
// Usage:
//
//	err := retry.Do(ctx, func(ctx context.Context) error {
//	    return callUpstream(ctx)
//	},
//	    retry.WithMaxAttempts(3),
//	    retry.WithBackoff(retry.ExponentialJitter(100*time.Millisecond)),
//	    retry.WithRetryIf(retry.IsTransient),
//	)
//
// The default strategy retries up to 3 times with exponential backoff
// starting at 100 ms and full jitter. Context cancellation always
// stops retries immediately.
package retry

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"
)

// Func is the function signature passed to Do.
type Func func(ctx context.Context) error

// BackoffFunc returns the duration to wait before attempt number n
// (1-based: first retry is n=2). The implementation must be safe for
// concurrent use.
type BackoffFunc func(attempt int) time.Duration

// RetryIfFunc decides whether an error is retryable. Return true to
// retry, false to abort immediately.
type RetryIfFunc func(err error) bool

// Options control retry behaviour. All fields have sensible defaults
// (see defaults in Do). Prefer the With* option functions.
type Options struct {
	MaxAttempts int
	Backoff     BackoffFunc
	RetryIf     RetryIfFunc
}

// Option is a functional option for Do.
type Option func(*Options)

// WithMaxAttempts sets the maximum total attempts (including the first
// call). Must be ≥ 1; values < 1 are treated as 1 (no retries).
func WithMaxAttempts(n int) Option {
	return func(o *Options) {
		if n >= 1 {
			o.MaxAttempts = n
		}
	}
}

// WithBackoff sets the backoff strategy. See ExponentialJitter,
// ConstantBackoff, and LinearBackoff helpers.
func WithBackoff(b BackoffFunc) Option {
	return func(o *Options) { o.Backoff = b }
}

// WithRetryIf sets the predicate that decides whether an error warrants
// a retry. Defaults to retrying on all non-nil errors (except context
// cancellation / deadline exceeded, which always abort).
func WithRetryIf(f RetryIfFunc) Option {
	return func(o *Options) { o.RetryIf = f }
}

// Do calls fn up to MaxAttempts times, sleeping between failures
// according to the Backoff strategy. It stops early when:
//   - fn returns nil (success)
//   - ctx is cancelled or deadline exceeded
//   - RetryIf returns false for the returned error
//   - MaxAttempts is exhausted
//
// The error from the last attempt is returned. If the context was
// cancelled, ctx.Err() is returned directly.
func Do(ctx context.Context, fn Func, opts ...Option) error {
	o := &Options{
		MaxAttempts: 3,
		Backoff:     ExponentialJitter(100 * time.Millisecond),
		RetryIf:     AlwaysRetry,
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.MaxAttempts < 1 {
		o.MaxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= o.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}
		// Context cancellation is never retried.
		if isContextError(lastErr) {
			return lastErr
		}
		if !o.RetryIf(lastErr) {
			return lastErr
		}
		if attempt == o.MaxAttempts {
			break
		}
		wait := o.Backoff(attempt)
		if wait <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return lastErr
}

// ─── Backoff strategies ───────────────────────────────────────────────────

// ExponentialJitter returns a BackoffFunc that computes
// random(0, min(cap, base × 2^(attempt-1))).
// This is the "Full Jitter" strategy from the AWS Architecture Blog.
// It collapses the thundering herd better than plain exponential backoff.
//
//	retry.ExponentialJitter(100*time.Millisecond)
//	  attempt 1 → random [0, 100ms]
//	  attempt 2 → random [0, 200ms]
//	  attempt 3 → random [0, 400ms]
//	  ...        capped at 30s by default
func ExponentialJitter(base time.Duration) BackoffFunc {
	return ExponentialJitterCapped(base, 30*time.Second)
}

// ExponentialJitterCapped is ExponentialJitter with a configurable cap.
func ExponentialJitterCapped(base, cap time.Duration) BackoffFunc {
	return func(attempt int) time.Duration {
		exp := math.Pow(2, float64(attempt-1))
		ceiling := float64(base) * exp
		if ceiling > float64(cap) {
			ceiling = float64(cap)
		}
		return time.Duration(rand.Float64() * ceiling)
	}
}

// ConstantBackoff returns a BackoffFunc that always waits d.
func ConstantBackoff(d time.Duration) BackoffFunc {
	return func(_ int) time.Duration { return d }
}

// LinearBackoff returns a BackoffFunc that waits base × attempt.
func LinearBackoff(base time.Duration) BackoffFunc {
	return func(attempt int) time.Duration { return base * time.Duration(attempt) }
}

// NoBackoff returns a BackoffFunc that never waits (immediate retry).
func NoBackoff() BackoffFunc { return func(_ int) time.Duration { return 0 } }

// ─── RetryIf predicates ───────────────────────────────────────────────────

// AlwaysRetry retries on every non-nil, non-context error.
func AlwaysRetry(err error) bool { return err != nil && !isContextError(err) }

// IsTransient is a RetryIfFunc that retries only on errors marked as
// transient via the Transient sentinel or via errors.As(*TransientError).
// Use it when only networking / temporary errors should be retried.
func IsTransient(err error) bool {
	var t *TransientError
	return errors.As(err, &t)
}

// TransientError marks an error as retryable. Wrap transient errors
// from the calling site; services like backoffcoord and circuitbreaker
// already produce these when the upstream is overloaded.
type TransientError struct{ Cause error }

func (e *TransientError) Error() string { return e.Cause.Error() }
func (e *TransientError) Unwrap() error { return e.Cause }

// Transient wraps err as retryable.
func Transient(err error) *TransientError { return &TransientError{Cause: err} }

// ─── helpers ──────────────────────────────────────────────────────────────

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
