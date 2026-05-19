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
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DefaultCheckTimeout is the per-check timeout applied when the
// suite is constructed without WithCheckTimeout.
const DefaultCheckTimeout = 5 * time.Second

// CheckFunc is the signature every selftest check satisfies. A nil
// return is a pass; a non-nil error message ends up in the response.
// The context passed in is a child of the request context with the
// suite's per-check timeout applied — honor it.
type CheckFunc func(ctx context.Context) error

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

// Suite is a registered set of named checks executed sequentially by
// Render. One suite per service is the intended pattern (held in a
// package-level var).
//
// Registration (Check) is safe for concurrent boot-time use. Render
// is safe to call concurrently — each invocation runs the checks
// against a fresh slice and writes its own response.
type Suite struct {
	service      string
	version      string
	checkTimeout time.Duration
	observer     Observer

	mu     sync.RWMutex
	checks []registered
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

// Check registers a named check in CategoryAny (matches all category
// filters). fn is invoked once per Render call, in registration order,
// with a child context that carries the suite's per-check timeout. Safe
// for concurrent registration at boot; do not register after the first
// Render — concurrent registration + iteration is undefined behaviour.
//
// Empty names and nil fns are silently dropped.
func (s *Suite) Check(name string, fn CheckFunc) {
	s.CheckIn(name, CategoryAny, fn)
}

// CheckIn registers a named check in the given category. Orchestrators
// can request only a specific category via ?category=readiness (or
// liveness, startup). Checks in CategoryAny always match.
func (s *Suite) CheckIn(name string, cat Category, fn CheckFunc) {
	if name == "" || fn == nil {
		return
	}
	s.mu.Lock()
	s.checks = append(s.checks, registered{name: name, fn: fn, category: cat})
	s.mu.Unlock()
}

// CheckResult is one row in the JSON response. Err is empty when
// Pass is true.
type CheckResult struct {
	Name     string   `json:"name"`
	Category Category `json:"category,omitempty"`
	Pass     bool     `json:"pass"`
	Err      string   `json:"err,omitempty"`
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

// Render runs registered checks (optionally filtered by ?category=)
// sequentially against a child context of r.Context() carrying the
// suite's per-check timeout, then writes the canonical JSON payload
// below to w.
//
// Query parameters:
//
//	?category=liveness|readiness|startup  — run only checks in that category
//	?live=1                               — sets IsLive(ctx) flag for checks
//
//	{
//	  "service": "<service>",
//	  "version": "<version>",
//	  "ok": true|false,
//	  "checks": [
//	    {"name": "<n>", "pass": true|false, "err": "<msg if !pass>"},
//	    ...
//	  ],
//	  "pass": N,
//	  "fail": N,
//	  "duration_ms": N
//	}
//
// HTTP semantics match the fleet smoke-gate convention:
//
//	200 OK             — every check passed (or no checks registered)
//	503 Service Unavailable — at least one check failed
//
// Content-Type: application/json. Each check is bounded by the
// per-check timeout; a check that exceeds it reports
// {pass: false, err: "timeout after <d>"} and the run continues so
// the rest of the suite still gets exercised. Concurrent callers do
// not share mutable state — each Render owns its own results slice.
func (s *Suite) Render(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx := withLive(r.Context(), q.Get("live") == "1")
	cat := Category(q.Get("category"))
	resp := s.run(ctx, cat)
	if s.observer != nil {
		s.observer.ObserveSelftest(Event{
			Service:  resp.Service,
			Version:  resp.Version,
			OK:       resp.OK,
			Pass:     resp.Pass,
			Fail:     resp.Fail,
			Duration: time.Duration(resp.DurationMs) * time.Millisecond,
			Checks:   resp.Checks,
		})
	}
	status := http.StatusOK
	if !resp.OK {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// Handler returns an http.Handler for the suite. Equivalent to
// http.HandlerFunc(s.Render) — provided for the
// `mux.Handle("/selftest", s.Handler())` style some services prefer.
func (s *Suite) Handler() http.Handler {
	return http.HandlerFunc(s.Render)
}

// run executes the checks (filtered by cat) and assembles the Response.
// Split out so tests can assert against a Response struct without an
// httptest.Recorder roundtrip.
func (s *Suite) run(parent context.Context, cat Category) Response {
	if parent == nil {
		parent = context.Background()
	}

	// Snapshot the slice under the read lock.
	s.mu.RLock()
	snapshot := make([]registered, len(s.checks))
	copy(snapshot, s.checks)
	s.mu.RUnlock()

	results := make([]CheckResult, 0, len(snapshot))
	pass, fail := 0, 0
	start := time.Now()

	for _, c := range snapshot {
		// Filter: run only checks that match the requested category.
		// CategoryAny on a check matches every filter.
		// CategoryAny filter (empty string) runs all checks.
		if cat != CategoryAny && c.category != CategoryAny && c.category != cat {
			continue
		}
		err := runOne(parent, c.fn, s.checkTimeout)
		cr := CheckResult{
			Name:     c.name,
			Category: c.category,
			Pass:     err == nil,
		}
		if err != nil {
			cr.Err = err.Error()
			fail++
		} else {
			pass++
		}
		results = append(results, cr)
	}

	return Response{
		Service:    s.service,
		Version:    s.version,
		OK:         fail == 0,
		Checks:     results,
		Pass:       pass,
		Fail:       fail,
		DurationMs: time.Since(start).Milliseconds(),
		Category:   cat,
	}
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
