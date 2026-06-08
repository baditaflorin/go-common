// Package loadshed provides a non-blocking concurrency gate ("load
// shedder") for bounding the number of in-flight calls a service makes
// to a slow, saturatable shared upstream.
//
// The recurring fleet failure it generalises: a service proxies to a
// slow sibling (go-js-proxy, go-html-proxy, any renderer) where each
// call holds a goroutine for tens of seconds. A backfill fan-out fires
// thousands of concurrent requests; they pile thousands of goroutines
// on the single saturated upstream; the scheduler thrashes; even
// trivial work — and downstream enrichers' /selftest probes — stall
// past their deadline, rolling back otherwise-healthy fleet deploys.
// (go_infrastructure_fetch_cache 0.3.8 / PR #12, observed 2026-06-08:
// ~8.3k goroutines, host loadavg 56/20 cores.)
//
// The cure is to cap concurrency and FAIL FAST on the excess rather
// than queue it: a non-blocking semaphore that admits up to `limit`
// callers and sheds the rest with an immediate 503 + Retry-After. The
// box stays responsive and the caller fails open instantly (degraded[]
// += "<upstream>-busy") instead of after a multi-second upstream
// timeout.
//
// Two usage shapes:
//
//	// 1. In-line — gate only the expensive sub-path of a handler
//	//    (e.g. only cache MISSES that reach the renderer):
//	gate := loadshed.New("render", limit) // limit<=0 => unbounded
//	...
//	if mustRender {
//	    release, ok := gate.TryAcquire()
//	    if !ok {
//	        loadshed.WriteShed(w, 0, "render capacity exceeded; retry shortly")
//	        return
//	    }
//	    defer release()
//	}
//	result := callSlowUpstream(...)
//
//	// 2. Middleware — gate a whole handler:
//	mux.Handle("/render", gate.Guard(0, "")(renderHandler))
//
// Metrics: wire promx.AutoWire (or promx.NewLoadshedCollectors) once at
// startup and every gate in the process emits loadshed_shed_total,
// loadshed_admitted_total, and loadshed_in_flight, labelled by
// {service, gate}. loadshed_shed_total is the canonical "you're shedding
// load" alert signal.
package loadshed
