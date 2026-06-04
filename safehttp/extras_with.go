package safehttp

import (
	"strings"
)

// WithTraceCollector configures auto-emit of call traces to
// go-fleet-call-tracer (ADR-0011). Each completed request POSTs a
// trace record to <url>/traces with the fields the tracer's POST
// /traces handler expects: {trace_id, span_id, from_service,
// to_service, method, path, status, duration_ms, ts}. Async,
// fire-and-forget — must NOT block the request. Failure to emit is
// logged at most once per minute (rate-limited) and silently dropped.
//
// Recommended: read CALL_TRACER_URL from env in main.go and pass
// here. If env unset, skip the option — safehttp falls through to
// current behaviour.
func WithTraceCollector(url string) Option {
	return func(o *options) { o.traceURL = strings.TrimRight(url, "/") }
}

// WithBackoffCoordinator configures consultation with
// go-fleet-backoff-coordinator (ADR-0013) before each retry attempt
// against a host that recently returned 5xx or 429. safehttp POSTs
// <url>/backoff with {host, last_response:{status, retry_after_header,
// ts}} and sleeps up to {wait_ms} (capped) in the response before the
// retry attempt. Coordinator outage = fall through to local backoff
// (current behaviour); never blocks indefinitely.
//
// Recommended: read BACKOFF_COORDINATOR_URL from env.
func WithBackoffCoordinator(url string) Option {
	return func(o *options) { o.backoffURL = strings.TrimRight(url, "/") }
}

// WithDegradedSink wires a caller-passed *[]string slice that gets
// "<callee-host>-down" appended on 5xx or network-timeout responses.
// The caller is expected to surface this in its own response (e.g.
// degraded[] in the JSON envelope) so consumers know which sibling
// silently fell back to local logic.
//
// Append is concurrency-safe (mu-protected internally). Caller owns
// the slice lifecycle and is responsible for resetting it per
// request.
//
// Recommended call site:
//
//	var degraded []string
//	c := safehttp.NewClient(safehttp.WithDegradedSink(&degraded), ...)
//	... handle the request, surface degraded in the response ...
func WithDegradedSink(sink *[]string) Option {
	return func(o *options) { o.degradedSink = sink }
}
