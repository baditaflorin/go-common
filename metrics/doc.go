// Package metrics provides a lightweight, in-process request statistics
// collector for JSON-served snapshots at /metrics/json.
//
// It tracks total requests, error counts, per-status-code counts,
// per-path counts, average/p50/p95/p99 latency, and runtime system
// stats (goroutines, heap). For Prometheus-format scraping, see promx.
package metrics
