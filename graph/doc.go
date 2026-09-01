// Package graph instruments fleet-wide service-to-service calls and
// emits them to go-fleet-graph. The collector aggregates edges into a
// queryable live graph; go-fleet-visualizer renders the result.
//
// Two chokepoints in go-common already see every fleet call:
//
//   - safehttp.NewClient wraps the outbound *http.Transport with
//     graph.RoundTripper, recording one outbound Event per call.
//   - server.New prepends graph.Middleware, recording one inbound
//     Event per served request.
//
// Both call Record under the hood. Services do not import this
// package directly; bumping go-common@vX.Y.Z is the entire rollout.
//
// Identity comes from server.Run via Init(serviceID, version). If
// Init is not called the package falls back to "unknown".
//
// Configuration is env-driven, read once at first use:
//
//	GRAPH_ENABLED        — default "false". Event emission is opt-in.
//	GRAPH_COLLECTOR_URL  — e.g. "https://fleet-graph.0exec.com". Remote
//	                       endpoints must use HTTPS; HTTP is loopback-only.
//	GRAPH_SAMPLE_RATE    — float 0..1, default 1.0.
//	GRAPH_API_KEY        — dedicated event-writer key sent as X-API-Key to
//	                       POST /events. Never falls back to FLEET_API_KEY.
//	GRAPH_READER_API_KEY — dedicated read key for Lookup only. It is never
//	                       used to write events.
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
//     never considered for graph transport.
//   - Endpoint-bound: remote graph requests require HTTPS and do not follow
//     redirects, so a collector cannot forward a graph credential elsewhere.
//   - Self-describing: every batch carries schema_version so the
//     collector can tolerate +1 evolution without coordinated deploys.
//   - No PII: path templating strips IDs/UUIDs/tokens before recording.
package graph
