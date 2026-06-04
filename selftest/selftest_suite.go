package selftest

import (
	"context"
	"encoding/json"
	"github.com/baditaflorin/go-common/safehttp"
	"net/http"
	"sync"
	"time"
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

	// Selftest validates the service's REAL outbound path (DNS + TLS +
	// origin), so disable fetch-cache delegate routing for every check's
	// safehttp request. Routing live probes through a cold fleet cache made
	// them slow enough to trip `fleet-runner deploy`'s 8 s smoke /selftest
	// timeout and false-fail otherwise-healthy deploys. Checks that wired an
	// explicit per-client WithFetchDelegate still route through it.
	parent = safehttp.WithoutFetchCacheContext(parent)

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
