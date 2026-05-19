// Package depcheck is the fleet's dependency-health registry. Services
// register the upstream(s) they depend on (html-proxy, js-proxy,
// rate-coordinator, the keystore, …) and depcheck probes them on every
// /health request. The probed status is folded into the /health JSON so
// operators (and the catalog UI) can see at a glance which transitive
// dependencies are degraded — not just whether the service's own process
// is up.
//
// Convention: registration happens at service startup; probes run on
// every /health call (no background polling, no caching). Probes are
// expected to be cheap — a HEAD request, an in-process check, a single
// TCP dial. Anything heavier should cache its own result.
//
// Sample wiring:
//
//	deps := depcheck.New()
//	deps.Register("html-proxy", func(ctx context.Context) error {
//	    return client.ProbeHTMLProxy(ctx)
//	})
//	server.Run("go_my_service", version, Handler,
//	    server.WithDependencies(deps))
//
// /health response then becomes:
//
//	{
//	  "status": "degraded",
//	  "service": "go_my_service",
//	  "version": "1.4.0",
//	  "dependencies": [
//	    {"name":"html-proxy","ok":true,"latency_ms":42,"checked_at":"..."},
//	    {"name":"keystore","ok":false,"latency_ms":3001,"error":"context deadline exceeded","checked_at":"..."}
//	  ]
//	}
//
// status is "healthy" if all probes pass, "degraded" if any fail.
// HTTP status code stays 200 either way — operators tail the JSON, not
// the status code — so liveness probes don't flap when a soft dep blips.
package depcheck

import (
	"context"
	"sync"
	"time"
)

// Probe is the cheap, idempotent check registered for an upstream.
// Return nil on success; any non-nil error is reported verbatim.
type Probe func(ctx context.Context) error

// Status is one row in the /health JSON for one registered dependency.
type Status struct {
	Name      string    `json:"name"`
	OK        bool      `json:"ok"`
	LatencyMs int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// Registry is the fan-out registry of probes.
type Registry struct {
	mu       sync.RWMutex
	entries  []entry
	timeout  time.Duration
	observer Observer
}

type entry struct {
	name  string
	probe Probe
}

// New creates an empty registry with a 2s per-probe timeout. Probes run
// in parallel on Snapshot(); 2s is the cap per probe, total Snapshot
// latency is therefore ~max(probe-latency) capped at 2s.
func New() *Registry {
	return &Registry{timeout: 2 * time.Second}
}

// WithTimeout overrides the per-probe timeout. Returns the receiver for
// chaining at service startup.
func (r *Registry) WithTimeout(d time.Duration) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timeout = d
	return r
}

// Register adds a named probe. Names should be short and stable
// ("html-proxy", "keystore"); they appear verbatim in /health JSON and
// in operator dashboards. Re-registering the same name appends a second
// entry — callers should register each dep exactly once.
func (r *Registry) Register(name string, probe Probe) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry{name: name, probe: probe})
}

// Snapshot fans out every registered probe in parallel with the
// per-probe timeout and returns one Status per registered dep.
// Order matches registration order.
//
// If the parent context cancels mid-probe, in-flight probes are
// reported as failed with the context error.
func (r *Registry) Snapshot(ctx context.Context) []Status {
	r.mu.RLock()
	entries := append([]entry(nil), r.entries...)
	timeout := r.timeout
	observer := r.observer
	r.mu.RUnlock()

	if len(entries) == 0 {
		return nil
	}

	out := make([]Status, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func(i int, e entry) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			err := e.probe(probeCtx)
			elapsed := time.Since(start)
			s := Status{
				Name:      e.name,
				OK:        err == nil,
				LatencyMs: elapsed.Milliseconds(),
				CheckedAt: time.Now().UTC(),
			}
			if err != nil {
				s.Error = err.Error()
			}
			out[i] = s
		}(i, e)
	}
	wg.Wait()
	if observer != nil {
		for _, s := range out {
			observer.ObserveDep(Event{
				Dep:     s.Name,
				OK:      s.OK,
				Latency: time.Duration(s.LatencyMs) * time.Millisecond,
				Err:     s.Error,
			})
		}
	}
	return out
}

// AllOK is a convenience for "should /health report healthy or degraded".
func AllOK(statuses []Status) bool {
	for _, s := range statuses {
		if !s.OK {
			return false
		}
	}
	return true
}
