// Package selftest gives every fleet service ONE canonical way to
// declare and serve a /selftest endpoint.
//
// Background — go-fleet-selftest-aggregator (ADR-0015) polls every
// service's /selftest and reduces the response to a status code +
// short body. Historically each service hand-rolled its handler and
// each one chose a slightly different JSON shape ({passed, failed} vs
// {pass_rate} vs 200-with-pass:false vs 503), forcing the aggregator
// to normalise the variance lossily. This package collapses all of
// that into one drop-in suite.
//
// Typical wiring (5 lines instead of 50):
//
//	var checks = selftest.NewSuite(ServiceID, Version)
//
//	func init() {
//	    checks.Check("upstream",  pingUpstream)
//	    checks.Check("resolver",  resolveOnce)
//	    checks.Check("embedded",  loadFixture)
//	}
//
//	http.HandleFunc("/selftest", checks.Render)
//
// Each check receives a child context with a 5 s timeout (override
// via WithCheckTimeout). Checks run sequentially in registration
// order. The handler emits the canonical JSON shape documented on
// Suite.Render and returns HTTP 200 if every check passed, 503 if
// any failed — matching the fleet convention used by the aggregator
// and by `fleet-runner deploy`'s smoke gate.
package selftest

import (
	"context"
	"fmt"
	"time"
)

// DefaultCheckTimeout is the per-check timeout applied when the
// suite is constructed without WithCheckTimeout.
const DefaultCheckTimeout = 5 * time.Second

// Option configures a Suite at construction time.
type Option func(*Suite)

// WithCheckTimeout overrides the default per-check timeout. Useful
// for services whose upstream calls can legitimately take longer
// than 5 s (e.g. a slow DNS-over-HTTPS provider) — but prefer
// fixing the underlying slowness over raising this knob.
func WithCheckTimeout(d time.Duration) Option {
	return func(s *Suite) {
		if d > 0 {
			s.checkTimeout = d
		}
	}
}

type registered struct {
	name     string
	fn       CheckFunc
	category Category
}

// NewSuite returns an empty Suite tagged with the calling service's
// id + version. Both strings are echoed back in every response so
// the aggregator can correlate /selftest output with the registry
// entry without parsing the URL.
func NewSuite(service, version string, opts ...Option) *Suite {
	s := &Suite{
		service:      service,
		version:      version,
		checkTimeout: DefaultCheckTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Response is the canonical /selftest payload. The aggregator
// (go-fleet-selftest-aggregator) treats it as opaque text but the
// shape is documented so a human reader and future tooling can
// rely on the field names.
type Response struct {
	Service    string        `json:"service"`
	Version    string        `json:"version"`
	OK         bool          `json:"ok"`
	Checks     []CheckResult `json:"checks"`
	Pass       int           `json:"pass"`
	Fail       int           `json:"fail"`
	DurationMs int64         `json:"duration_ms"`
	// Category echoes the ?category= filter used, or empty for all.
	Category Category `json:"category,omitempty"`
}

// runOne wraps a single check with a per-check timeout context. The
// timeout deadline is enforced two ways: the child context carries
// the deadline (well-behaved checks notice and return ctx.Err()),
// and a watchdog goroutine surfaces a synthetic timeout error if
// the check ignores its context. The runaway goroutine is allowed
// to keep running — we cannot kill it from outside — but the
// suite's response is not blocked on it.
func runOne(parent context.Context, fn CheckFunc, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- fmt.Errorf("panic: %v", rec)
			}
		}()
		done <- fn(ctx)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timeout after %s", timeout)
	}
}
