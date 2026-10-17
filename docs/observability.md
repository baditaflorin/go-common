# Observability: which package emits what

go-common has several packages that put a runtime signal where an operator or
another program can read it. They share a vocabulary (most of them say
"metrics") but not a job. This page says which one to use, and what
`server.New` has already switched on for you.

It was written against v0.102.71 and checked against the code, not against the
package comments. Where the two disagree the code wins, and the disagreement is
listed under [Known limits](#known-limits).

## Start here

| I want to... | Use | What I have to do |
| --- | --- | --- |
| Chart request rate, errors and latency of my service | [`promx`](../promx/) | Nothing. `server.New` records every request; scrape `GET /metrics`. |
| Add a counter or gauge of my own | [`promx`](../promx/) | Register it on `promx.Registry()` **after** `server.New` returned (see [Known limits](#known-limits)). |
| See where one slow request spent its time | [`reqstats`](../reqstats/) | Read the `Server-Timing` / `X-Request-Stats` response headers; add phases with `defer reqstats.From(ctx).Phase("name")()`. |
| Follow one request across services | [`telemetry`](../telemetry/) | Propagation is always on; spans are exported only once an OTLP endpoint or the fleet vault credential is configured. |
| Search logs by service, version, trace id | [`logx`](../logx/) | `LOG_FORMAT=json` or `server.WithStructuredLogs()`. Off by default. |
| Find a leak or a CPU hot spot in one live process | [`obs`](../obs/) | Call `obs.Init()` in `main` (loopback pprof; before v0.102.74 it had to come after `server.New`, see [Known limits](#known-limits)). For continuous profiles use [`profiling`](../profiling/), see [profiling.md](profiling.md). |
| Know which services call which | [`graph`](../graph/) | `GRAPH_ENABLED=true` plus a collector URL and writer key. Off by default. |
| See what environment a process is really running with | [`env`](../env/) registry | The start-up check already logs problems; `/debug/config` on the `obs` debug server lists declared variables, redacted; [ENV.md](ENV.md) lists them all. |
| Read a snapshot with `curl`, with no Prometheus | [`metrics`](../metrics/) | Nothing; `GET /metrics/json` (behind whatever auth the service installed). See [the `metrics` question](#is-metrics-superseded-by-promx). |

The packages measure different things. `promx` and `metrics` are aggregates over
all requests; `reqstats` is one request, in that request's response; `telemetry`
is one request's path through several services; `graph` is the topology; `logx`
and the access log are events. Pick by the question, not by the word "metrics".

## What `server.New` wires

The request path, outermost first (from `Server.Handler` in
`server/server_server.go` and the middleware list in `server/server.go`):

1. `telemetry.HTTPMiddleware`: extracts `traceparent`, starts a server span. It
   skips `/health`, `/healthz`, `/readyz`, `/livez`, `/metrics`, `/metrics/json`,
   `/version` and `/selftest`.
2. `wrapDefaults`: answers `GET /metrics` (the `promx` registry) and `GET /selftest`
   itself unless you registered your own route for them. These two never enter
   the chain below, so no auth applies and nothing below counts them.
3. `fetchSourceMiddleware`: not a signal, but a `400` for a bad `?source=` is
   returned here, before anything below sees the request.
4. The default chain, in slice order: `reqstats`, `RequestID`, `Logging` (the
   access log), the body limit, `metrics`, `promx` HTTP. Your own middleware
   (for example `WithKeystoreAuth`) comes next, so a `401` is still counted by
   all of the above. `graph.Middleware` is last: it only sees requests that got
   through your middleware.
5. The mux, with `/health`, `/readyz`, `/version`, `/metrics/json`, `/capabilities`
   and `/schema` registered.

At construction, once: `runtimetune.Apply`, the environment check (it only logs),
`graph.Init`, `telemetry.Init`, `promx.AutoWire` (creates the collector sets and
installs the process-wide observers listed below), and structured logs if asked.
In `Start()`: `profiling.StartFromEnv`, and on shutdown a flush of `graph` and
`telemetry`.

| Signal | Default in `server.New` | Switch |
| --- | --- | --- |
| `promx` HTTP collectors, `/metrics` | **on**, always | none (your own `/metrics` route replaces the endpoint) |
| `metrics` JSON, `/metrics/json` | **on**, always | none |
| `reqstats` headers | **on** | `server.WithoutRequestStats()` |
| Access log (`middleware.Logging`) | **on**, plain JSON on stdout | `LOG_SKIP_PATHS` demotes prefixes to DEBUG; no off switch |
| `telemetry` propagation and middleware | **on** | none; `OTEL_DISABLED=true` stops export only |
| `telemetry` span export | **on only if configured**: `OTEL_EXPORTER_OTLP_ENDPOINT`, or a vault key / ingestion token file, which defaults the endpoint to the fleet's OpenObserve. Root sampling 0.1. | `OTEL_DISABLED=true`, `OTEL_SAMPLE_RATE` |
| `graph` middleware | installed; **records only if** `GRAPH_ENABLED=true`, collector URL and writer key are all set | unset `GRAPH_ENABLED` |
| `logx` structured logs | **off** | `LOG_FORMAT=json` / `server.WithStructuredLogs()` turn it on; `LOG_LEVEL` |
| `env` start-up check | **on** (logs problems, never stops the service) | none |
| `profiling` (Pyroscope) | **off** until `PYROSCOPE_SERVER_ADDRESS` is set | unset the variable |
| `obs` debug server (pprof, `/metrics` mirror, `/debug/config`) | **off**: `server.New` never starts it (`server/obs_optin_test.go` guards this) | call `obs.Init()`; `DEBUG_ADDR=off` or `OBS_DISABLE=1` disable it again |

`promx.AutoWire` also installs process-wide observers, and `server.New` sets the
`safehttp` egress observer, so these emit with no further wiring: `safehttp`
egress and backoff, `degraded`, `response.Envelope`, `fleetfetch`,
`circuitbreaker`, `workpool`, `loadshed`, `backoffcoord` and `policyeval`.
`WithKeystoreAuth` attaches the auth collectors. The cgroup (`go_container_*`) and
`graph` sender counters are read when `/metrics` is scraped. All of these are
event-driven or scrape-time, so they add nothing to a request that does not use
the primitive.

## Decision matrix

| Package | What it emits, and where | Read by | In `server.New` | Use it when |
| --- | --- | --- | --- | --- |
| [`promx`](../promx/) | Prometheus text (OpenMetrics when the scraper asks) at `GET /metrics` on the service port, **unauthenticated**. `http_requests_total{service,method,route,status}`, `http_request_duration_seconds`, `http_response_size_bytes`, `http_requests_in_flight`; Go runtime and process collectors; `build_info`; `go_container_*` ([cgroup-resource-metrics.md](cgroup-resource-metrics.md)); the primitive families above. A sampled request adds a `trace_id` exemplar to the duration histogram. | A Prometheus scraper | Always on | You want anything aggregated over time: rate, error ratio, latency quantiles, saturation, alerts. This is the default answer for every service. |
| [`metrics`](../metrics/) | JSON at `GET /metrics/json`, through the middleware chain, so behind whatever auth middleware the service installed (`WithKeystoreAuth` does not exempt it): totals, errors, status counts, per-path counts (at most 1000 paths, then `_other`), average and p50/p95/p99 over the last 10,000 requests, five coarse latency buckets, goroutines and heap, `last_request`. | A person or script with `curl` and a credential. No reader in this repository. | Always on (`Server.Stats`) | Almost never for new work. See [below](#is-metrics-superseded-by-promx). |
| [`reqstats`](../reqstats/) | `Server-Timing` and `X-Request-Stats` (JSON: service, version, `ok`, `total_ms` to the first byte, `bytes.in` from `Content-Length`, `bytes.out` only if the handler reports it, named phases, an `approx.proc_cpu_ms`, optional `render` and nested `upstream`) on every response that goes through the chain, error responses included. | The caller: `curl`, devtools, a calling service (`SetUpstream` nests the callee's header), cost attribution ([ADR-0001](adr/0001-per-request-resource-stats.md)). | On; `WithoutRequestStats()` | You need the cost of one request, not of all requests. `approx` is a process-wide `getrusage` delta and over-attributes under concurrency: a hint, never a bill. `WithHeapDelta` (a stop-the-world per request) is not reachable through `server.New`. |
| Access log: [`middleware.Logging`](../middleware/) | One `request_completed` line per request on stdout: request id, method, path (no query), remote address, status, duration, bytes; a per-request logger reachable with `middleware.LoggerFromContext`. | The log aggregator reading container stdout | On | Always there. Quiet probe noise with `LOG_SKIP_PATHS=/health,/readyz,/metrics`. |
| [`logx`](../logx/) | Makes slog JSON the process logger, and by `slog.SetDefault` also the standard `log` package: service, version, node, `trace_id` and `span_id` inside a traced request, credentials redacted. | The log aggregator | Off; opt in | You want every log line structured and correlated with traces, and you accept the cost below. |
| [`telemetry`](../telemetry/) | W3C `traceparent` propagation, an inbound server span and an outbound client span (`safehttp`, `meshclient`), exported over OTLP/HTTP. Spans carry method and status only: no URL, query, headers or body. | An OTLP backend (the fleet's OpenObserve) | Propagation always; export when configured | You want to follow a request across services or see a slow dependency. Keep span names and attributes low-cardinality, see its package doc. |
| [`graph`](../graph/) | One event per inbound request that reaches it (caller `unknown` unless verified) and per outbound call, batched to the fleet graph collector; paths are templated (`/{id}`, `/{uuid}`, `/{token}`). | The graph collector, which aggregates it into a service-to-service map | Installed; records only when enabled | You want topology, not performance. Its volume is evidence of an edge, not a request count. |
| [`obs`](../obs/) | A loopback-only HTTP server (default `127.0.0.1:6060`): `/debug/pprof/*`, a `/metrics` mirror of the `promx` registry, `/debug/config` (declared environment variables, redacted). | An operator on the host or over a tunnel | Never started | A live process is leaking, spinning or misconfigured and you need a profile now. No per-request cost. |
| [`profiling`](../profiling/) | Continuous CPU, allocation and goroutine profiles pushed to Pyroscope; no listener ([profiling.md](profiling.md)). | A Pyroscope server | Started in `Start()` if configured | You want profiles over time and across deploys rather than one on demand. |
| [`env`](../env/) registry | A start-up log line per problem (unparsable value, removed variable still set, likely typo); the redacted list at `/debug/config`; [ENV.md](ENV.md). | An operator | Check runs on every start | You own environment variables: declare them with `env.Declare`. |

Two rules fall out of the matrix. Aggregates go through `promx`; per-request
detail goes through `reqstats` and `telemetry`; do not add a third surface for
either. And the only default a service can switch off is `reqstats`: everything
else on the default path has no off switch (the cost is in the next section).

## What it costs per request

The numbers are medians of five runs on one shared build host (AMD Ryzen 9 3900,
the container limited to 8 CPUs, Go 1.25, load average 3 to 5), around a handler
that writes two bytes, through `httptest.NewRecorder`. Time is noisy on a shared
host (the `promx` layer ranged from 0.85 to 1.16 microseconds across runs);
allocations are exact and are what `scripts/bench-gate.sh` gates.

The benchmarks that are in the repository, which you can run yourself with
`go test -run '^$' -bench . -benchmem ./server ./promx ./reqstats ./middleware`:

| Benchmark | Time | Bytes | Allocs |
| --- | --- | --- | --- |
| `server.BenchmarkServerRequest` (the whole default stack) | 10.1 us | 5,873 | 79 |
| `promx.BenchmarkHTTPMiddleware` | 0.64 us | 243 | 6 |
| `reqstats.BenchmarkMiddleware` | 3.6 us | 2,289 | 19 |
| `middleware.BenchmarkRequestID` | 1.0 us | 1,360 | 13 |
| `middleware.BenchmarkChainRequestIDLoggingMetrics` (output discarded) | 5.0 us | 2,954 | 36 |

The per-layer split below was measured with a throwaway harness that wraps each
middleware alone around the same handler (it is not in the repository). Each row
is the extra cost over the bare handler (0.26 us, 6 allocations, 274 bytes):

| Layer | On by default | Extra time | Extra allocs | Extra bytes |
| --- | --- | --- | --- | --- |
| Access log, output discarded | yes | 3.1 us | 22 | 1.5 KB |
| Access log through `logx` | no | 8.3 us (5.2 us more than plain) | 23 | 1.7 KB |
| `reqstats` | yes | 4.0 us | 15 | 2.1 KB |
| `reqstats` with `WithHeapDelta` | no, not reachable via `server.New` | 49 us | 15 | 2.1 KB |
| `telemetry` middleware, no provider installed | yes | 1.6 us | 23 | 1.2 KB |
| `telemetry` middleware, sampled span, no exporter | when sampled (10% of roots by default) | 2.5 us | 25 | 1.7 KB |
| `RequestID` | yes | 1.3 us | 9 | 1.2 KB |
| `promx` HTTP | yes | 0.8 us | 2 | 36 B |
| `metrics` JSON | yes | 0.24 us | 1 | 32 B |
| `graph`, disabled | yes | 0.24 us | 1 | 128 B |
| `graph`, enabled | no | about 1.5 us (noisy) | 10 | 0.4 KB |

What this says:

- The default stack costs about 10 microseconds and 79 allocations per request
  around a handler that does nothing. For a handler that takes a millisecond that
  is 1%; it only matters for very cheap, very hot endpoints.
- The biggest defaults are the access log, `reqstats` and `telemetry`. Switching
  `reqstats` off (`WithoutRequestStats`) took the full stack from 10.9 to 6.5
  microseconds and from 77 to 65 allocations in the same harness. Its cost is two
  `getrusage` calls, a JSON marshal and two header writes per request (read from
  the code, not profiled).
- `promx` and `metrics` together cost about one microsecond. The duplication
  between them is not a performance problem.
- `telemetry` costs 23 allocations even when nothing is exported, because the
  middleware still extracts context, starts a non-recording span and wraps the
  writer. That is the price of keeping propagation on.
- Turning on `logx` more than doubles the access-log line (3.3 to 8.5 us). The
  redaction patterns run on every string attribute; this was not profiled.
- The output write of the access log (a `write` to stdout per request) is not in
  these numbers.
- `obs`, `profiling` and the `env` registry have no per-request hook. `profiling`
  samples continuously in the background; its overhead was not measured here.
- `GET /metrics/json` is more expensive than it looks: a snapshot copies and
  sorts up to 10,000 samples and calls `runtime.ReadMemStats` (about 120
  microseconds and 160 KB per call with a full window). Do not poll it every
  second from many places.

## Is `metrics` superseded by `promx`?

**Not deprecated.** `promx` records the same facts, but not all of them, and
nothing can show that no consumer reads the JSON.

Where `server` uses `metrics.Stats` (this is all of it; no other package in the
repository does):

1. `server.New` creates `metrics.New()`, exposes it as the exported field
   `Server.Stats`, and installs `middleware.Metrics(stats)` (itself exported API
   that takes a `*metrics.Stats`) in the default chain.
2. `GET /metrics/json` returns `stats.Snapshot()` as JSON.

What `promx` gives for each field of that JSON:

| `/metrics/json` field | In `promx` |
| --- | --- |
| `total_requests`, `total_errors` | `sum(http_requests_total)`, and the same with `status=~"4..\|5.."` |
| `status_counts` | `http_requests_total` by `status` |
| `path_stats` | `http_requests_total` by `route` (cap 512, `_other` beyond it; the JSON caps at 1000) |
| `latency.avg_duration` | `http_request_duration_seconds_sum / _count` |
| `latency.buckets` (<10ms, <100ms, <500ms, <1s, >1s) | the `le="0.01"`, `0.1`, `0.5`, `1` buckets (default buckets include all four; `le` is inclusive, the JSON's bound is not) |
| `latency.p50`, `p95`, `p99` | `histogram_quantile`, interpolated inside the default buckets rather than exact over the last 10,000 requests |
| `system.goroutines`, `heap_alloc`, `stack_in_use` | `go_goroutines`, `go_memstats_heap_alloc_bytes`, `go_memstats_stack_inuse_bytes` |
| `start_time`, `uptime` | `process_start_time_seconds` |
| `last_request` | **nothing**. There is no last-request timestamp in `promx`. |
| the JSON itself | **nothing**. `promx` serves Prometheus text; there is no scraper-free structured view. |

Checked on a running `server.New` handler: every `promx` name in the table is
present in `/metrics`. `promx` also has things the JSON lacks: response sizes,
in-flight requests, per-method breakdown, exemplars.

Who depends on the JSON: of the 18 local repositories that import go-common
(see the census below), none imports `metrics` and none uses `Server.Stats`. In
a search of every local clone (source, configuration and Markdown files under
1 MB), the string `/metrics/json` appears only in copies of go-common's own
package table and in one service that does not import go-common and serves a
different JSON of its own at that path. No script, configuration or dashboard
reads it. That is a lower bound: it says nothing about the other services, the
operators' habits, or tooling outside these clones.

So the two conditions for deprecating are not met: `promx` does not cover
`last_request`, exact percentiles or a scraper-free JSON view, and the absence of
readers is not established. Keeping `metrics` costs a fraction of a microsecond
per request and an endpoint that has no HTTP-level test in the repository. To
decide, measure rather than guess: count `GET /metrics/json` in the fleet's access logs (the access log
records `path`) over a week. If it is zero apart from checks by hand, and the
team accepts losing `last_request` (or adds a `last_request` gauge to `promx`,
which is additive), add this to the `metrics` package comment, with removal not
before two minor releases later, as [api-policy.md](api-policy.md) requires:

```go
// Deprecated: use promx (GET /metrics). Removal no earlier than v0.104.0.
```

Removing the package would also remove `Server.Stats` and `middleware.Metrics`,
which is BREAKING.

## Usage census

Counts of distinct repositories, found by searching the local clones of the
fleet, that import each package directly in non-test code. Dated and worktree
copies of one service count once. **This is a lower bound**: the fleet has about
110 services and not all are cloned; 46 clone directories collapse to 18
repositories that import go-common at all. Switches that are environment
variables (`LOG_FORMAT`, `GRAPH_ENABLED`, `OTEL_*`, `PYROSCOPE_SERVER_ADDRESS`,
`DEBUG_ADDR`) live in deployment configuration, which the census does not read.

| Package | Repositories importing it directly |
| --- | --- |
| `server` | 12 (11 call `server.New`, 3 call `server.Run` or `RunTier`) |
| `env` | 9 (none uses the `env.Declare` registry) |
| `telemetry` | 5 |
| `graph` | 5 |
| `middleware` | 4 |
| `promx` | 2 (both use `promx.Registry()` or `NewEgressCollectors` for their own metrics) |
| `metrics` | 0 |
| `reqstats` | 0 |
| `obs` | 0 |
| `profiling` | 0 |
| `logx` | 0 (new in v0.102.71) |

None of the repositories that import `server` uses an observability option
(`server.WithoutRequestStats`, `server.WithStructuredLogs`, `promx.WithRouteFunc`,
`WithRouteLimit`) or calls `reqstats`, `obs.Init` or `profiling.StartFromEnv`
itself. They take what `server.New` gives them, which is why the defaults above
matter more than the options.

## Signals and result types

[outcomes.md](outcomes.md) says which type carries the answer of one call. The
signals here are the aggregate view of the same facts, and they connect like this:

| Result type | How it shows up as a signal |
| --- | --- |
| `errors` (status and code) | The status in `http_requests_total{status}`, `X-Request-Stats` `ok` (false on 5xx) and the access log. The stable `code` is only in the body. |
| `degraded` tokens | `fleet_degraded_total{primitive,suffix}` (automatic through `promx.AutoWire`), plus the `degraded` list in the response envelope. `meshclient` in fail-open mode appends them for you. |
| `meshresult` | No automatic counter. Log it or count it yourself on `promx.Registry()`. |
| `resilience` (breaker, load shed, retries) | The `circuitbreaker` and `loadshed` families, through `AutoWire`. |

Keep any label or span attribute you add low-cardinality: a status, a primitive
name, an outcome; never an id, a URL or a user.

## Known limits

Found against v0.102.71 while writing this page and listed so that an author is
not surprised. An item says when a later release fixed it; the rest are still true.

Wiring order:

- `obs.Init()` before `server.New` left the debug server's `/metrics` mirror
  empty for the life of the process: it captured the registry that existed when
  it started, and `promx.Init` had not run. `server.Run` calls `New` and `Start`
  itself, so the right order could only be had from a custom `server.Option`
  (options run after the collectors are wired). pprof was not affected.
  **Fixed in v0.102.74:** the mirror looks the registry up on every scrape, so
  `obs.Init()` can come before or after `server.New`.
- `promx.Registry()` and `promx.Handler()` before `promx.Init` hand back a
  throwaway registry. Anything you register on it is never served. Register
  after `server.New` returns.
- Calling `promx.NewHTTPCollectors(nil)` (or another constructor `AutoWire` already
  ran) on the shared registry after `server.New` panics with "duplicate metrics
  collector registration attempted".
- The default `/metrics` serves the private `promx` registry, not
  `prometheus.DefaultRegisterer`: metrics a service registers with `promauto`
  or the default registry do not appear on it.
- Two `server.New` in one process share the `promx` collectors but each has its
  own `metrics.Stats`, so the Prometheus and JSON views stop agreeing.

Label cardinality (`promx` `http_*`):

- The `route` label is the raw `r.URL.Path`, capped at 512 distinct values; the
  first 512 seen keep their own series, everything later is `_other` for the
  life of the process. `server.New` offered no way to change this:
  `AutoWire` takes no options, `HTTPCollectors` has no setter, and
  `WithRouteFunc` / `WithRouteLimit` only work on a collector you build
  yourself. The comment on `Server.PromHTTPCollectors` suggested otherwise.
  **Since v0.102.74** `server.WithPromRoute(fn, limit)` sets the route function
  and the limit for one server, and the comment points to it. The default is
  unchanged (the raw path, 512), so a service with parameterised paths has to
  opt in.
- The `method` label was `r.Method` with no cap, and Go's server accepts any
  token as a method. A probe sending 300 distinct methods to one path created 300
  new series sets; after about 900 requests the scrape was 2.0 MB and 22,000
  lines (about 27 lines per method-and-route pair). Reachable by anyone who can
  reach the port, before your auth middleware runs. **Fixed in v0.102.74:** a
  method other than GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS, CONNECT and
  TRACE is labelled `OTHER`.
- Because `/metrics` is unauthenticated, an identifier or token embedded in a
  request path becomes a label value readable there. `graph` templates paths
  (`/{id}`, `/{uuid}`, `/{token}`) and `promx` does not. `WithPromRoute` is the
  way to return a template instead.
- `fleet_degraded_total{primitive,suffix}` comes from whatever string is passed
  to `degraded.Sink.Append`, uncapped. The host labels are capped (256), and
  `pool`, `gate`, `dep`, `check`, `ruleset`, `upstream` come from names in code:
  keep them constants.

Overlap and blind spots:

- Every request is counted by both `metrics` and `promx`, by design.
- `promx` and `metrics` count probe traffic (`/health`, `/readyz`,
  `/metrics/json`). `telemetry` and `graph` each keep their own list of probe
  paths and the lists differ: with graph emission on, `/readyz`, `/metrics/json`
  and `/schema` each produced a graph event, although the `graph` package says
  probes are excluded.
- A call that the fetch-cache delegate answers is counted in the `safehttp`
  egress family (`channel="cache_hit"`) and in the `fleetfetch` family. Do not
  add the two.
- Requests rejected by `fetchSourceMiddleware`, and the built-in `/metrics` and
  `/selftest`, never reach the access log, `reqstats`, `metrics` or `promx`.

Other:

- `reqstats` puts the service id and version in a header of every response that
  goes through the chain, error responses included. `bytes.out` is zero unless the
  handler sets `Content-Length` or calls `SetBytesOut`.
- Importing `obs` links `net/http/pprof`, whose `init` registers handlers on
  `http.DefaultServeMux`. A service that serves the default mux on a public
  listener must not import `obs`. (Go's behaviour; not tested here.)
- Only `graph`, `runtimetune` and `LOG_FORMAT` / `LOG_LEVEL` are declared in the
  `env` registry. `DEBUG_ADDR`, `OBS_DISABLE`, `OTEL_*`, `PYROSCOPE_*` and
  `LOG_SKIP_PATHS` are not, so neither the start-up check nor `/debug/config`
  knows about them and a typo in one is silent.
- The package comment of `promx` said `telemetry.Init` calls `AutoWire`. It does
  not; only `server.New` (or a direct call) does. **Fixed in v0.102.74** (the
  comment).
