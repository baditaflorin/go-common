// Package graph instruments fleet-wide service-to-service calls and
// emits them to go-fleet-graph. The collector aggregates edges into a
// queryable live graph; go-fleet-visualizer renders the result.
//
// The standard outbound and inbound chokepoints in go-common observe fleet calls:
//
//   - safehttp.NewClient wraps the outbound *http.Transport with
//     graph.RoundTripper, recording one outbound Event per call.
//   - fleetfetch's internal cache client uses graph.RoundTripper while
//     keeping its Docker-DNS request outside the environment proxy.
//   - server.New runs graph.Middleware inside authentication middleware,
//     recording one inbound Event per request with a verified service
//     principal. Unauthenticated or generic principals are recorded as
//     "unknown".
//
// Inbound identity is never inferred from User-Agent. The keystore middleware
// marks a verified principal; custom HTTP servers may opt in to X-Auth-User
// only from exact gateway IPs configured with GRAPH_TRUSTED_CALLER_IPS.
// When an inbound server span is active, Middleware copies that same resolved
// identity to fleet.caller.id and records fleet.caller.verified. Unverified
// callers use the fixed value "unknown"; arbitrary request headers and user
// names are never copied into span attributes.
// Outbound events derive caller identity from the running service itself.
//
// When both ends are instrumented, one end-to-end HTTP request can emit both
// an outbound and inbound Event. go-fleet-graph aggregates those emitted
// observations as topology evidence; its volume is not a unique-request,
// user, prompt, or LLM-invocation count.
//
// Both call Record under the hood. Services get automatic coverage by using
// the standard go-common server and safehttp client. An outbound caller that
// addresses a catalogued service through an ambiguous shared IP should attach
// graph.WithTarget(ctx, serviceID) to that request context so the emitted
// edge keeps its canonical logical destination.
//
// Identity comes from server.Run via Init(serviceID, version), or from
// GRAPH_SERVICE_ID when that is set. Events recorded before either has
// supplied it (a secrets lookup or poller started ahead of server.New) are
// held without an owner and stamped with the identity when they are sent; the
// sender ships nothing until the identity is known. If Init is never called
// those events are never sent, because the collector rejects a whole batch
// containing an event that does not belong to the authenticated writer.
//
// A batch the collector refuses for its content (403, 400, 413, 415, 422) is
// dropped and counted in Counters.BatchesRejected instead of being retried
// forever; 401, 408, 429 and 5xx keep the batch for a later flush.
//
// Configuration is env-driven, read once at first use:
//
//	GRAPH_ENABLED        — default "false". Event emission is opt-in.
//	GRAPH_SERVICE_ID     — optional canonical registry ID supplied by the
//	                       fleet-rendered Compose overlay. It overrides a
//	                       legacy Go package name for writer identity.
//	GRAPH_COLLECTOR_URL  — exactly "https://fleet-graph.0exec.com" for a
//	                       remote collector. HTTP/HTTPS loopback endpoints
//	                       are allowed only for local development and tests.
//	GRAPH_SAMPLE_RATE    — float 0..1, default 1.0.
//	GRAPH_API_KEY_FILE   — optional protected file containing the dedicated
//	                       event-writer key. When set, it takes precedence
//	                       and read failures fail closed without env fallback.
//	GRAPH_API_KEY        — compatibility env source for the dedicated writer
//	                       key. Never falls back to FLEET_API_KEY.
//	GRAPH_READER_API_KEY — dedicated read key for Lookup only. It is never
//	                       used to write events.
//	GRAPH_TARGET_ALIASES - optional comma-separated
//	                       go-fleet-*=fleet-* mappings for known bare
//	                       Docker hostnames; it changes telemetry only.
//	GRAPH_TRUSTED_CALLER_IPS — optional comma-separated literal IP allowlist
//	                            for custom gateways stamping X-Auth-User.
//	GRAPH_BUFFER_SIZE    — ring capacity (default 10000 events).
//	GRAPH_FLUSH_INTERVAL — flush cadence in seconds (default 10).
//	GRAPH_FLUSH_BATCH    — max events per flush (default 500).
//
// Design rules:
//
//   - Opt-in: event recording and emission require GRAPH_ENABLED=true, a
//     valid collector URL, and GRAPH_API_KEY. Missing configuration makes no
//     request and records no event.
//   - Fail-open: collector failures never block callers. One failed batch is
//     retained for the next scheduled retry; later batches remain queued.
//   - Async: Record never blocks the calling request.
//   - Bounded: the ring buffer plus at most one failed batch caps memory;
//     oldest ring events drop first when the buffer is full.
//   - Scoped: writers and readers use separate keys; a broad FLEET_API_KEY is
//     never considered for graph transport. File-backed writer keys take
//     precedence over the compatibility environment source.
//   - Endpoint-bound: remote graph requests target only the canonical HTTPS
//     graph host and do not follow redirects, so an environment override or
//     collector response cannot forward a graph credential elsewhere.
//   - Self-describing: every batch carries schema_version so the
//     collector can tolerate +1 evolution without coordinated deploys.
//   - No PII: path templating strips IDs/UUIDs/tokens before recording.
//   - Alias-safe: GRAPH_TARGET_ALIASES is an explicit bounded mapping for
//     internal Docker aliases. It never rewrites a request, accepts no URL,
//     port, path, or domain, and malformed entries retain ordinary external
//     target classification.
//   - Context-aware identity: WithTarget annotates only the graph event for
//     one request. It does not rewrite the URL or affect request routing.
//   - Bounded shutdown: Shutdown accepts an optional context and caps the
//     final flush at five seconds; callers may impose a shorter deadline.
package graph
