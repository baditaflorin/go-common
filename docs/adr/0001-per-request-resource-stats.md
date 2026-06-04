# ADR-0001 (go-common) — Per-request resource stats (`reqstats`)

* **Status**: Proposed
* **Date**: 2026-06-04
* **Authors**: claude-opus-4-8 (with florin)
* **Tags**: go-common, telemetry, reqstats, server-timing, cost-attribution, chromedp, render
* **Scope**: design + schema + cost model + rollout. Build order: this ADR → go-common `reqstats` → go-js-proxy (reference) → js-proxy-network, html-proxy, fetch-cache.

## Context

The fleet's render path (`consumer → fetch-cache → go-js-proxy | go-js-proxy-network | go-html-proxy`) has no per-request resource visibility. We want, **embedded in each response**:

1. **Instant debugging** — "what did *this* request cost, and where did the time go?" without grepping container metrics.
2. **Cost attribution** — assign different prices to requests by the resources they actually consumed.

The naive approach (return the container's CPU%/RSS) is wrong, and getting this right hinges on one fact:

**For render services the cost lives in Chromium, not the Go process.** A render barely touches the Go process — it shuttles CDP messages while the *browser tab* burns CPU and RAM. So:

| Signal | Per-request? | Accurate under concurrency? | Use |
|---|---|---|---|
| Container CPU% / RSS (e.g. `go-js-proxy 338%`) | no | n/a (cgroup-wide) | ambient context only |
| Go-process `getrusage` CPU / heap-alloc delta | sort of | **no** — process-wide, over-attributes under load | rough debug only, never billing |
| Go-side **phase wall-times** (queue/render/parse/serialize) | **yes** | yes | debug + latency |
| **CDP `Performance.getMetrics`** per page | **yes** | **yes** | **the cost basis** |
| Byte sizes (in/out/upstream) | yes | yes | cost + debug |

CDP `Performance.getMetrics` returns, *for that tab*: `ScriptDuration` + `TaskDuration` + `LayoutDuration` + `RecalcStyleDuration` (renderer **CPU seconds** for this page), `JSHeapUsedSize`/`JSHeapTotalSize` (**memory**), `Nodes`, `Documents`, `LayoutCount`, `Frames`. We already have the browser open — this is the accurate per-request render cost, free.

This belongs in **go-common**, not four bespoke implementations (the "change the library, not the consumers" rule). It complements the existing `metrics`/`promx`/`telemetry` packages (which are *aggregate*); `reqstats` is the *per-request, response-embedded* view.

## Decision (proposed)

Add a go-common **`reqstats`** package that:

1. Times named phases of a request.
2. Collects an optional **`render`** block (CDP metrics) and an optional, clearly-labeled **`approx`** block (Go-process deltas).
3. **Nests** a callee's stats under `upstream`, so one fetch-cache response shows the whole chain.
4. Emits two response headers:
   * **`Server-Timing`** (W3C) — human/devtools-readable phase + total durations.
   * **`X-Request-Stats`** — the full canonical JSON, for the cost engine.

Each of the four services fills what it has; the two chromedp services add `render`; the cache nests `upstream`. Cost attribution is built on the **accurate** signals (CDP renderer time + bytes + phases) — the `approx` block is **never** the cost basis.

## Canonical schema (`X-Request-Stats`, compact JSON)

```json
{
  "svc": "go-js-proxy",
  "ver": "0.5.1",
  "ok": true,
  "total_ms": 2480,
  "phase": { "queue_ms": 8, "boot_ms": 0, "fetch_ms": 2410, "parse_ms": 44, "serialize_ms": 3 },
  "bytes": { "in": 0, "out": 131072, "upstream": 131072 },
  "render": {
    "script_ms": 910, "task_ms": 1700, "layout_ms": 120, "recalc_style_ms": 60,
    "js_heap_used": 48211000, "js_heap_total": 67108864,
    "nodes": 5400, "documents": 3, "layout_count": 18, "frames": 2
  },
  "approx": {
    "proc_cpu_ms": 35, "heap_alloc_delta": 2200000,
    "note": "process-wide getrusage/alloc deltas — over-attributed under concurrency; NOT for billing"
  },
  "upstream": { "...": "nested X-Request-Stats from the service this one called" }
}
```

* `render` — chromedp services only (go-js-proxy, go-js-proxy-network). Absent for go-html-proxy (no browser) and the cache.
* `approx` — present only when `EnableApprox()` is called; always carries `note`.
* `upstream` — present when the service called another fleet renderer; recursion is one level per hop (the cache's upstream is the renderer; the renderer has no further fleet hop). To bound header size, deep chains may summarize `upstream` to `{svc, ver, total_ms, render.task_ms, bytes.out}`.

`Server-Timing` is derived from `phase` + `total` + key render metrics, e.g.:
`Server-Timing: queue;dur=8, render;dur=2410, cpu;dur=1700;desc="chromium task", extract;dur=44, total;dur=2480`

## go-common `reqstats` API (sketch)

```go
import "github.com/baditaflorin/go-common/reqstats"

func handler(w http.ResponseWriter, r *http.Request) {
    rt := reqstats.Start(ServiceID, Version) // starts total timer
    rt.EnableApprox()                         // opt-in getrusage + ReadMemStats deltas
    defer rt.Write(w)                         // emits Server-Timing + X-Request-Stats

    done := rt.Phase("queue"); /* acquire slot */ done()
    done = rt.Phase("fetch");  /* render */      done()

    rt.SetRender(reqstats.Render{ScriptMs: ..., TaskMs: ..., JSHeapUsed: ...}) // chromedp
    rt.SetUpstream(callee.Header.Get("X-Request-Stats"))                       // nest
    rt.AddBytesOut(n)
}
```

Helpers: `reqstats.Middleware(next)` for zero-touch phase=`total` + headers; `reqstats.Parse(headers)` to read a callee's stats for nesting; `reqstats.RenderFromCDP(metrics []performance.Metric) Render` (a thin mapper kept in a build-tagged or separate sub-helper so go-common core doesn't import chromedp — the chromedp dependency stays in the render services, which pass already-extracted metric values into `reqstats.Render`).

**chromedp dependency boundary:** go-common MUST NOT import chromedp/cdproto. The render services call `Performance.getMetrics` themselves and hand the plain numbers to `reqstats.Render{}`. go-common only defines the struct + emission.

## Measurement details

* **Phases** — `time.Now()`/`time.Since`. Trivial, exact.
* **CDP render** — `performance.Enable()` before navigate, `performance.GetMetrics()` after extract (before tab close). Durations are float **seconds** → ×1000 for ms. One extra CDP round-trip per render (negligible).
* **`approx.proc_cpu_ms`** — `syscall.Getrusage(RUSAGE_SELF)` utime+stime delta across the request. **Process-wide** → contaminated by concurrent requests. Labeled, debug-only.
* **`approx.heap_alloc_delta`** — `runtime.ReadMemStats().TotalAlloc` delta. Also process-wide cumulative. Labeled, debug-only.
* **bytes** — counted at read/write.

## Cost model (built on the accurate signals only)

```
request_cost ≈ w_cpu · render.task_ms          // dominant for renders (Chromium CPU)
             + w_js  · render.script_ms
             + w_mem · render.js_heap_used
             + w_net · bytes.out
             + w_lat · total_ms                 // covers non-render services
             + upstream.cost                     // nested renderer cost attributed to the caller
```

`w_*` calibrated to real $/core-second and $/GB. For go-html-proxy / the cache (no `render`), cost ≈ phases + bytes; their dominant cost is the nested `upstream` renderer. The `approx` block is deliberately **absent** from this formula.

## Rollout

1. **ADR** (this).
2. **go-common `reqstats`** — package + `Server-Timing`/`X-Request-Stats` emit + `Parse` + tests. No consumer change; tagged minor release.
3. **go-js-proxy reference** — wire phases (queue/boot/fetch/extract/serialize) + `render` (CDP) + `approx`; verify the headers + schema live against the container. Locks the schema.
4. **Roll out** — go-js-proxy-network (CDP + network counts), go-html-proxy (phases + bytes, no `render`), fetch-cache (phases + `SetUpstream` nesting). `fleet-runner update-dep` the go-common bump, then per-service integration PRs.
5. **(Later)** optional fire-and-forget to a stats sink for aggregation + a calibration job that fits `w_*` from observed cost.

## Alternatives considered

* **Four bespoke implementations.** Rejected — drift + duplicated effort; this is the textbook "change go-common" case.
* **Only `/metrics` (Prometheus).** Aggregate, not per-request; can't debug or price a single request. `reqstats` complements it.
* **OpenTelemetry spans.** Heavier; the fleet already has a call-tracer + `telemetry` package. `reqstats` is the lightweight response-embedded view; it can export to OTel later.
* **Container/cgroup per-request accounting.** Impossible — cgroup stats are per-container.
* **Go-process CPU/RAM as the cost basis.** Rejected for billing — concurrency-contaminated and misses the Chromium cost. Kept only as labeled `approx`.

## Risks & open questions

* **Header size** for deep `upstream` nesting → summarize beyond one hop; `X-Request-Stats` stays compact ASCII JSON (base64 only if an escaping issue surfaces).
* **`approx` misused for billing** → mitigated by the embedded `note` + keeping it out of the documented cost formula + naming.
* **CDP `getMetrics` failure** must not fail the render → best-effort; omit `render` on error.
* **Privacy** — stats carry no URL/PII, only sizes/durations; safe to log.

## Consequences

* go-common gains a fleet-wide `reqstats` package; every adopting service's responses carry `Server-Timing` (devtools-debuggable) + `X-Request-Stats`.
* A **defensible per-request cost basis** (Chromium CPU + bytes), enabling differential pricing.
* One-glance per-request debugging across the whole render chain via the nested envelope.
