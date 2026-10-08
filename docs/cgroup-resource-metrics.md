# Go service container resource metrics

`go-common` v0.102.35 adds cgroup resource snapshots and registers them through
`promx.AutoWire` / the standard `go-common/server` startup path. A service that
uses that startup path and exposes `/metrics` publishes `go_container_*`
signals for its **own process cgroup**.

Signals include current memory use and limit, available memory when limited,
cumulative CPU seconds, CPU quota in cores, throttled periods/time, and Linux
PSI CPU/memory pressure where the kernel exposes it. Unsupported or unlimited
values are omitted; `go_container_resource_read_error` indicates a failed
snapshot. The static `service` label is the Go Common service ID.

Example PromQL:

```promql
go_container_memory_usage_bytes
go_container_memory_available_bytes
rate(go_container_cpu_usage_seconds_total[5m])
rate(go_container_cpu_throttled_seconds_total[5m])
go_container_cpu_pressure_some_avg10_ratio
go_container_memory_pressure_full_avg10_ratio
```

This is application-container telemetry. It does not report the host's total
capacity, sibling containers, a Woodpecker workflow, or a BuildKit build. Use
node exporter / Proxmox capacity metrics for hosts and the
`go-fleet-metrics-hub` step-container sampler for individual Woodpecker jobs.
The sampler is host-side because it must inspect multiple short-lived Docker
containers; moving that collector into each Go service would not give it the
needed visibility.

To consume these metrics in a service, use `go-common` v0.102.35 or later,
ensure the normal `server.New` or `promx.AutoWire` path is used, and verify the
service's `/metrics` endpoint and Prometheus target. Older service versions do
not emit these series until their dependency is bumped and redeployed.
