# ADR-0001 (go-common) — Per-request resource stats (`reqstats`), universal

* **Status**: Proposed
* **Date**: 2026-06-04
* **Authors**: claude-opus-4-8 (with florin)
* **Tags**: go-common, server, middleware, telemetry, reqstats, server-timing, cost-attribution
* **Scope**: design + schema + cost model + rollout. Build order: this ADR → go-common `reqstats` (default server middleware) → go-js-proxy (reference enrichment) → fleet-wide via dep bump.

## Context

Every Go service in the fleet burns CPU to execute, some do network I/O to fetch
things, all spend wall-time and bytes — yet **none of that is visible per
request**. We want, embedded in *every* response (regardless of what it returns):

1. **Instant debugging** — "what did *this* request cost and where did the time
   go?" straight from the response, no metric-grepping.
2. **Cost attribution** — price requests by the resources they actually consumed.

This must be **universal and zero-config**: baked into `go-common/server` so any
service using `server.New(...)` emits it for free on the next dep bump — not four
(or 220) bespoke integrations. The render services then *enrich* it; they are a
special case, not the design center.

The honest measurement picture, now split by service shape:

| Signal | Per-request? | Accurate under concurrency? | Where it applies |
|---|---|---|---|
| Container CPU% / RSS | no | n/a (cgroup-wide) | ambient context only |
| **Wall-time** (total + named phases) | **yes** | yes | **every service** |
| **Bytes** in / out / upstream | **yes** | yes | **every service** |
| Go-process `getrusage` CPU + heap-alloc delta | sort of | **no** — process-wide, over-attributes | every service, `approx` block, debug-only |
| CDP `Performance.getMetrics` (per tab) | **yes** | **yes** | chromedp services only — `render` block |

The uncomfortable truth: **for a generic Go service there is no clean
per-request CPU number.** The runtime doesn't expose per-goroutine CPU, goroutines
migrate across OS threads, and `getrusage(RUSAGE_SELF)` is process-wide (so it
over-attributes under load). What we *do* have accurately, everywhere, is
**wall-time + bytes**; the process deltas are a labeled hint. Render services are
the lucky exception — Chromium reports per-tab CPU/heap exactly via CDP.

This belongs in go-common alongside `metrics`/`promx`/`telemetry` (which are
*aggregate*); `reqstats` is the *per-request, response-embedded* view, and it
slots into the existing default-middleware chain in `server.New`.

## Decision (proposed)

Add a go-common **`reqstats`** package wired as a **default server middleware
(on by default)**. With zero per-service code, every response gains:

* **`Server-Timing`** (W3C) header — readable in devtools/curl.
* **`X-Request-Stats`** — the canonical JSON envelope for the cost engine.

The automatic (universal) payload is **`total_ms` + `bytes` + `approx`** (the
process deltas). Services opt into richer data via a request-scoped tracker:
named **`phase`** timings, the **`render`** block (chromedp), and **`upstream`**
nesting (callers of other fleet services). Opt out per-service with
`server.WithoutRequestStats()`.

Cost attribution is built on the **accurate** signals — wall-time + bytes
everywhere, CDP renderer CPU where it exists — never the `approx` block.

## Universal by default — what every service gets, and how to enrich

**Free (no code):** the default middleware times the whole request, counts
bytes, samples the process deltas, and writes both headers. Turn it off with
`server.WithoutRequestStats()` (rare).

**Enrich (opt-in), via the tracker on the request context:**

```go
rt := reqstats.From(r.Context())      // the middleware put it there
done := rt.Phase("db");  /* query */  done()
done = rt.Phase("upstream"); resp := call(); done()
rt.SetUpstream(resp.Header.Get("X-Request-Stats"))   // nest the callee's stats
rt.SetRender(reqstats.Render{TaskMs: ..., JSHeapUsed: ...})  // chromedp services
```

**Public exposure:** `Server-Timing` is a standard, safe-to-expose header.
`X-Request-Stats` reveals internal timings/heap sizes — harmless on the internal
mesh (where cost attribution happens), but for *public* vhosts the gateway should
strip it (one nginx `proxy_hide_header X-Request-Stats;` on external server
blocks). Default: emit always; document the gateway strip. (Alternative knob:
emit `X-Request-Stats` only when an `X-Debug: 1`/internal header is present —
deferred unless wanted.)

## Canonical schema (`X-Request-Stats`, compact JSON)

```json
{
  "svc": "go-js-proxy", "ver": "0.5.1", "ok": true,
  "total_ms": 2480,
  "bytes": { "in": 0, "out": 131072, "upstream": 131072 },
  "approx": {
    "proc_cpu_ms": 35, "heap_alloc_delta": 2200000,
    "note": "process-wide getrusage/alloc deltas — over-attributed under concurrency; NOT for billing"
  },
  "phase":  { "queue_ms": 8, "fetch_ms": 2410, "parse_ms": 44, "serialize_ms": 3 },
  "render": { "script_ms": 910, "task_ms": 1700, "layout_ms": 120, "js_heap_used": 48211000, "nodes": 5400 },
  "upstream": { "...": "nested X-Request-Stats from the service this one called" }
}
```

`total_ms`, `bytes`, `approx` are always present (universal). `phase`, `render`,
`upstream` are present only when the service enriches. Deep chains summarize
`upstream` beyond one hop to bound header size.

`Server-Timing` is derived from `total` + `phase` (+ key render metrics), e.g.:
`Server-Timing: total;dur=2480, fetch;dur=2410, cpu;dur=1700;desc="chromium task"`.

## Measurement details

* **Wall-time / phases** — `time.Since`. Exact, universal.
* **`approx.proc_cpu_ms`** — `syscall.Getrusage(RUSAGE_SELF)` utime+stime delta;
  process-wide → contaminated under concurrency. For *low-QPS* services it's a
  decent estimate; still `approx`.
* **`approx.heap_alloc_delta`** — `runtime.ReadMemStats().TotalAlloc` delta;
  process-wide cumulative. Cheap; `approx`.
* **`render`** — chromedp services call `performance.Enable()` before navigate,
  `performance.GetMetrics()` after extract; float seconds → ms. go-common
  **must not** import chromedp — services pass plain numbers into
  `reqstats.Render{}`; the package only defines the struct + emission.
* **`bytes`** — counted at the middleware's wrapped `ResponseWriter` + request body.

## Cost model — two paths, both on accurate signals

* **Render services (direct):** Chromium's per-tab CPU is exact —
  `cost ≈ w_cpu·render.task_ms + w_js·render.script_ms + w_mem·render.js_heap_used + w_net·bytes.out + upstream.cost`.
* **Generic services (proportional):** no clean per-request CPU, so attribute the
  container's *measured* CPU (from cgroup/`/metrics`) across requests **weighted
  by each request's accurate `total_ms` share** in the window:
  `req_cpu ≈ container_cpu_window · (req.total_ms / Σ total_ms)`, then
  `cost ≈ w_cpu·req_cpu + w_net·bytes.out + upstream.cost`. This uses two accurate
  inputs (per-request wall-time + container CPU) to derive a defensible per-request
  CPU without needing per-goroutine CPU. The `approx` block is a sanity-check, not
  the basis.

`w_*` calibrated to real $/core-second and $/GB.

## Rollout

1. **ADR** (this).
2. **go-common `reqstats`** — package + default middleware in `server.New`
   (+ `WithoutRequestStats` opt-out) + `Server-Timing`/`X-Request-Stats` emit +
   `From`/`Parse` + tests. On the next dep bump, **every service** starts emitting
   the universal payload with no code change.
3. **go-js-proxy reference** — enrich with `phase` + `render` (CDP) + verify the
   schema/headers live. Locks the enrichment shape.
4. **Fleet-wide** — `fleet-runner update-dep github.com/baditaflorin/go-common@vX`
   lights up the universal payload everywhere; enrich high-value services
   (js-proxy-network, fetch-cache nesting) incrementally.
5. **(Later)** optional stats sink for aggregation + a calibration job for `w_*`;
   gateway `proxy_hide_header X-Request-Stats` on public vhosts.

## Alternatives considered

* **Per-service bespoke stats.** Rejected — 220 integrations, guaranteed drift.
  The whole point is one middleware in go-common.
* **Only `/metrics` (Prometheus).** Aggregate, not per-request; can't debug or
  price one request. `reqstats` complements it (and the generic cost path *reuses*
  the container CPU that `/metrics` already exposes).
* **OpenTelemetry spans everywhere.** Heavier; per-request response-embedding is
  lighter and devtools-native. `reqstats` can export to OTel later.
* **Per-goroutine CPU via `RUSAGE_THREAD` + `LockOSThread`.** Pins each request to
  an OS thread for the handler — defeats the scheduler, real overhead. Rejected;
  the proportional model gets defensible CPU without it.
* **Go-process deltas as the cost basis.** Rejected for billing
  (concurrency-contaminated); kept as labeled `approx`.

## Risks & open questions

* **Header size** on deep `upstream` nesting → summarize beyond one hop; keep
  `X-Request-Stats` compact ASCII JSON.
* **`approx` misused for billing** → embedded `note` + excluded from the cost
  formula + naming.
* **Public info-disclosure** of `X-Request-Stats` → default-on + gateway strip on
  external vhosts (or the `X-Debug` gate if preferred).
* **Default-on is a fleet-wide behavior change** (new headers on every response) →
  harmless (additive headers), opt-out available; roll via the normal dep bump.
* **Middleware overhead** must be negligible (two `time.Now`, one `getrusage`, one
  `ReadMemStats`) — `ReadMemStats` has a small STW cost; if it shows up, sample it
  or use a cheaper alloc counter.

## Consequences

* go-common gains a fleet-wide `reqstats` middleware; **every** service's responses
  carry `Server-Timing` (devtools-debuggable) + `X-Request-Stats` after one dep
  bump, no per-service code.
* A defensible per-request cost basis for *both* render (CDP) and generic
  (proportional) services.
* One-glance per-request debugging across any service, and across the whole chain
  via nested `upstream`.
