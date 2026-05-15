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
//	GRAPH_ENABLED        — default "true". "false" disables entirely.
//	GRAPH_COLLECTOR_URL  — e.g. "https://go-fleet-graph.0exec.com".
//	GRAPH_SAMPLE_RATE    — float 0..1, default 1.0.
//	GRAPH_API_KEY        — fleet key sent as X-API-Key to the collector.
//	GRAPH_BUFFER_SIZE    — ring capacity (default 10000 events).
//	GRAPH_FLUSH_INTERVAL — flush cadence in seconds (default 10).
//	GRAPH_FLUSH_BATCH    — max events per flush (default 500).
//
// Design rules:
//
//   - Fail-open: if the collector is unreachable, drop events silently.
//   - Async: Record never blocks the calling request.
//   - Bounded: ring buffer caps memory; oldest events drop first.
//   - Self-describing: every batch carries schema_version so the
//     collector can tolerate +1 evolution without coordinated deploys.
//   - No PII: path templating strips IDs/UUIDs/tokens before recording.
package graph
