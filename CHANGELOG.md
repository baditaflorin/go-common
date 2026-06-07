# Changelog

All notable changes to `github.com/baditaflorin/go-common` are recorded here.
Versioning follows semver on the git-tag axis; the package itself has no
embedded version string (consumers pin via `go.mod`).

## Unreleased

### Changed

- **`promx` inbound HTTP metrics now cap "route" label cardinality** (default
  512, configurable via `WithRouteLimit`). The default `RouteFunc` returns the
  raw `r.URL.Path`, which is wired unconditionally into every `server.New`
  service's default middleware — so parameterised paths (`/users/42`) or
  scanner traffic produced an **unbounded** number of `http_requests_total`
  / `http_request_duration_seconds` / `http_response_size_bytes` series,
  growing the in-process Prometheus client AND the scrape server's memory
  without limit (a latent fleet-wide OOM vector, default-on). Routes beyond
  the cap now fold to the literal `_other`, reusing the same `hostCardCap`
  already protecting the egress/backoff/fleetfetch host labels. Services with
  fewer than 512 distinct routes see **no change**; the only services affected
  were already emitting unbounded series. Pass `WithRouteFunc` for proper
  templated routes and/or `WithRouteLimit(n)` to raise the cap.

### Added

- **`promx.WithRouteLimit(n)`** — overrides the route-label cardinality cap
  (above). A value `<= 0` restores the default of 512.

### Performance

- **`metrics.Stats` is now lock-free on the request path.** `Record` (a default
  middleware on every `server.New` service) previously took a process-global
  `sync.Mutex` and did several map writes per request — a fleet-wide contention
  point that also duplicated the bookkeeping `promx` already does with
  atomic Prometheus vecs. The running totals moved into an internal atomic
  accumulator (counters, a fixed status-code array, latency buckets, and a
  lock-free ring for percentile samples); `New`, `Record`, and `Snapshot` keep
  their exact signatures and `/stats` keeps its exact JSON. Also bounds
  `PathStats` to 1000 distinct paths (overflow → `_other`), closing the same
  raw-`r.URL.Path` cardinality hazard fixed for the promx route label.
- **`promx` HTTP middleware caches the `http_requests_in_flight` gauge** curried
  to the constant `service` label, removing two `WithLabelValues` map
  lookups+locks per request (it was resolved on both the `Inc` and the deferred
  `Dec`). Per-request win on the universal `server` path.
- **`safehttp` reuses one shared HTTP client for fleet coordinator/tracer
  POSTs** (backoff consult + trace emission). Both paths previously built a
  fresh `http.Client`+`http.Transport` with `DisableKeepAlives` on every call,
  so a service degraded against an upstream opened a brand-new TCP(+TLS)
  connection to the coordinator per request. Now pooled with keep-alives;
  per-call deadlines still enforced via the request context.
- **`reqstats` allocates the per-request `phases` map lazily** (on first
  `Mark`/`Phase`) instead of in `Start`. Most requests record no phases, so
  this drops one map allocation per request on the universal server path.
- **`safehttp.GuardHost` caches definitive verdicts** (allowed / `ErrBlocked`)
  for hostnames with a short 30s TTL, bounded to 8192 hosts. The SSRF guard
  was re-resolving the same host on every `CheckURL` validation call and again
  in the dialer; the cache removes that duplication. **Not a security
  regression:** the `Dialer.Control` re-check still validates the actually-
  connected IP independently, so a stale "allowed" verdict can never let a
  connection reach a blocked address. Transient DNS failures are never cached.

### Tooling

- **`scripts/build-modules.sh`** + pre-commit wiring — builds **and** vets
  every module in the repo, including nested ones. The root `go build ./...`
  / `fleet-runner build-test` do not descend into the `telemetry/` nested
  module, so a root-package change that breaks `telemetry.go` would pass the
  root gate and silently break the nested module until a future consumer
  pulled it. The pre-commit hook now runs this script after the gitleaks
  scan, so every module is gated locally. No importable-code change — does
  not require a version bump. New nested modules are picked up automatically.

## v0.62.0 — 2026-06-08

### Changed

- **`telemetry` is now a nested module** (`github.com/baditaflorin/go-common/telemetry`
  with its own `go.mod`). It was the only package pulling the OpenTelemetry SDK +
  gRPC + protobuf + genproto + grpc-gateway tree, and a fleet-wide audit found
  **zero** of ~438 consumers import it. Carving it into its own module removes that
  whole tree from the root module: external dependency **packages 202 → 64**,
  **`go.sum` 94 → 52 lines**, **build-list modules 77 → 42**. Every consumer drops
  ~138 transitive packages / ~35 modules from its build graph, `go.sum`, module
  cache, and supply-chain/CVE surface on its next `go mod tidy` — **no code change
  and no import-path change** (the import path is unchanged; only a separate
  `require` is needed by the handful of future callers that want OTel tracing).
  This is a build-graph/download win, not a binary-size win for current services
  (none link telemetry today). Note: `telemetry/` is now excluded from the root
  `go test ./...` / `go build ./...`; release tooling should build it separately.

### Added

- **`safehttp.WithMaxIdleConnsPerHost(n)`** + raised default. `safehttp` clients
  left `Transport.MaxIdleConnsPerHost` at the Go std default of **2**, which
  throttled connection reuse for the common fleet shape (a service hammering one
  upstream API/sibling) — every request past the 2nd in flight churned a fresh
  TCP+TLS handshake. The default is now **10** (`defaultMaxIdleConnsPerHost`,
  capped below `MaxIdleConns`=20); hot single-upstream callers can raise it
  further via the new option. A value `<= 0` restores the default.

## v0.61.0 — 2026-06-07

### Added

- **`obs`** — fleet-canonical runtime observability. Two pieces:
  - **Localhost-only debug server** (`obs.StartDebugServer(addr)` /
    `obs.Init()` / `obs.MustInit()`) serving `net/http/pprof`
    (`/debug/pprof/*`) plus a `/metrics` mirror of the shared promx
    registry, **bound to `127.0.0.1` by design** so pprof's heap/CPU
    surface is reachable in-container or over an SSH tunnel but never
    from the public gateway. Returns a clean `StopFunc`; disabled (no-op)
    when the address is empty/`off`. Env knobs: `DEBUG_ADDR` (default
    `127.0.0.1:6060`), `OBS_DISABLE=1` / `DEBUG_ADDR=off` to turn it off.
  - **`obs.RegisterRuntimeMetrics(reg)`** — idempotent,
    `AlreadyRegistered`-safe registration of the Go runtime + process
    collectors (`go_goroutines`, `go_memstats_*`, `go_gc_duration_seconds`,
    `process_resident_memory_bytes`). A no-op for `server`-based services
    (`promx.Init` already registered them on the shared registry); exists
    for services that build their own `*prometheus.Registry`.
- **`server.New` auto-wires the obs debug server** at construction, gated
  by `DEBUG_ADDR` / `OBS_DISABLE` (default ON, loopback-only). Every fleet
  service gets pprof for diagnosing RSS creep / goroutine leaks before an
  OOM with **zero per-service code** — adoption is automatic on the next
  `go-common` bump. A bind failure is logged, never fatal; the listener is
  released on `Start()`'s shutdown paths. Opt out with `DEBUG_ADDR=off`.
  +8 self-tests. No new deps (reuses the existing `prometheus/client_golang`
  and stdlib `net/http/pprof`).

## v0.60.0 — 2026-06-04

### Added

- **`testhelpers/fakeexec`** — in-memory fake for the canonical command-runner
  shell-out seam (`run(dir, name string, args ...string) (string, error)`) that
  fleet apps use to invoke git / go / docker. Lets any app hermetically
  self-test command *orchestration* — which commands run, in what order, and how
  the code reacts to their exit status — with no real toolchain and no network.
  Records calls, scripts per-command output/errors (`OnReturn` / `FailOn`),
  runs side-effect hooks (`OnDo`, e.g. a faked `go mod vendor` that writes
  `vendor/modules.txt`), and ships `AssertCalled` / `AssertNotCalled` /
  `AssertOrder` / `Count`. Stdlib-only — adds nothing to any consumer's module
  graph. First adopter: `go_fleet_runner`'s `update-dep` vendoring tests. +13
  self-tests.

## v0.58.0 — 2026-06-04

### Added

- **`fleetfetch` distinct metric results for `WithoutCache`** — direct
  (cache-bypass) fetches now emit `fleet_fetch_total{result="direct"}` on
  success and `result="direct_error"` on failure, instead of reusing
  `"fallback"`/`"error"`. By-design cache-bypass traffic (e.g. a docs
  detector's speculative `developer.<domain>.com` probes) is now clearly
  separable from real cache activity on dashboards — a cache error panel no
  longer misreads direct-fetch failures as cache errors. New `Event.Result`
  values documented in `observer.go`. +1 test.

## v0.57.0 — 2026-06-04

### Added

- **`fleetfetch.WithoutCache()`** — a per-client option that skips the fetch
  cache entirely: every `Get` goes straight to the SSRF-safe, proxy-aware
  direct fetch (the `directFetch` path, which honors `HTTP(S)_PROXY` for
  `proxy_egress` services). For services that probe speculative,
  mostly-nonexistent, or one-shot URLs (e.g. a docs-platform detector
  guessing `developer.<domain>.com` for every input) — routing those through
  the cache pays a Docker-internal round-trip AND pollutes the shared cache +
  its singleflight with throwaway lookups no other service will reuse. The
  `Response` shape is unchanged (`ViaFallback=true` marks the direct path);
  `WithRender` has no effect under `WithoutCache` (a direct fetch returns raw
  origin bytes). This is the fleetfetch-side complement to safehttp's
  `WithoutFetchCache()` for services that use `fleetfetch.NewClient` directly.

## v0.55.0 — 2026-06-04

### Added

- **`safehttp.WithoutFetchCacheContext(ctx)`** — a per-request opt-out of
  fetch-cache delegate routing. Eligible GETs made with the returned context
  go direct to origin instead of through the process-wide
  `DefaultFetchDelegate`, even on a normal cache-routing client and even on
  clients built before `server.New` installed the delegate. Complements the
  per-client `WithoutFetchCache()` option. An explicit per-client
  `WithFetchDelegate` still wins (the caller wired it on purpose).

### Changed

- **`selftest.Suite` now runs every check with `WithoutFetchCacheContext`** —
  `/selftest` validates the service's REAL outbound path (DNS + TLS + origin),
  not whatever the fleet cache happens to have warm. Previously, live-probe
  selftest checks routed through the *cold* fleet cache and could exceed
  `fleet-runner deploy`'s 8 s smoke `/selftest` timeout, false-failing
  otherwise-healthy deploys and rolling them back (observed across the
  fetch-cache rollout, e.g. `domain-deployment-fingerprint`'s
  `live_probe_cross_platform`). Checks now bypass the cache automatically — no
  per-service code change needed. Checks that wired an explicit per-client
  `WithFetchDelegate` are unaffected.

## v0.47.2 — 2026-06-04

### Added

- **`safehttp` fetch-cache routing debug log** (`SAFEHTTP_FETCHCACHE_DEBUG=1`) —
  logs the per-GET routing decision (`perClientDelegate` / `useDefaultFetchCache`
  / `defaultDelegateInstalled` / `willRoute`) and outcome (`routed via cache
  host=… status=…` vs `delegate fell through to direct host=… err=…`),
  rate-limited ~once/2s, off by default. A diagnostic for confirming a service
  routes its egress through the fleet fetch cache. Used to prove in-situ routing
  on a live canary: page-fetch sources route through the cache (with real
  hits/misses), while pathological upstreams (e.g. crt.sh) fall through
  gracefully to direct egress. The per-consumer `fleet_fetch_*{service,host}`
  metrics are already wired process-wide via `promx.AutoWire`, so a service's
  cache usage is visible without per-client wiring.

## v0.47.1 — 2026-06-04

### Fixed

- **`safehttp` fetch-cache routing now reaches package-level clients** — the
  process-wide `DefaultFetchDelegate` is resolved **at call time** in the
  transport (mirroring `DefaultObserver`), not baked in at `NewClient`. In
  v0.47.0 a client constructed before `server.New` installed the delegate
  (the common case: a service builds its `safehttp` client in a package-level
  `var` at import time) silently bypassed the cache and kept fetching origin
  directly. Caught by a live canary (`subdomain-finder` still hit crt.sh /
  hackertarget directly). `WithoutProxy` / `WithoutFetchCache` opt-outs and
  per-client `WithFetchDelegate` are unchanged. Regression test added.

## v0.47.0 — 2026-06-04

### Added

- **`safehttp` fetch-cache routing — transparent fleet-wide GET dedup** —
  a new `FetchDelegate` hook lets eligible outbound GETs route through the
  fleet fetch-cache (`fleetfetch`) instead of hitting origin directly, so
  many services fetching the same URL collapse to one origin fetch
  (server-side singleflight + caching). New API: `safehttp.FetchDelegate`
  / `FetchResult`, `SetDefaultFetchDelegate` / `DefaultFetchDelegate`,
  `WithFetchDelegate` (per-client), `WithoutFetchCache` (per-client opt-out).
  `server.New` auto-installs a `fleetfetch`-backed delegate when
  `FLEET_FETCH_CACHE_URL` is set — **zero per-service code changes**; flip
  the env var + bump the dep to enable fleet-wide.
  - Only plain GETs (no body, no `Range`) are routed; any delegate error or
    nil result **falls through** to the normal direct path — a cache outage
    never breaks a request.
  - `WithoutProxy` (SSRF probers / direct-egress clients) and
    `WithoutFetchCache` clients are NOT routed through the cache — they keep
    real origin egress. An explicit `WithFetchDelegate` still applies.
  - Delegated GETs intentionally skip the `safehttp_egress_*` observer (the
    fetch happens in the cache, which emits its own `fleet_fetch_cache_*`
    metrics) — so origin egress visibly shifts from per-service to the cache.
  - `safehttp` does not import `fleetfetch` (the adapter lives in `server`),
    preserving the one-directional dependency.

## v0.42.0 — 2026-05-31

### Added

- **`safehttp.WithForceHTTP2()` — reliable negotiated ALPN** — sets
  `Transport.ForceAttemptHTTP2` so the client offers `h2` in the TLS
  ClientHello ALPN and upgrades to HTTP/2 against capable origins.
  - Needed because `NewClient` installs a custom `DialContext` (the SSRF
    guard); per net/http semantics a custom dialer "conservatively
    disables HTTP/2", so by default no `h2` is offered and
    `resp.TLS.NegotiatedProtocol` comes back empty even for HTTP/2-capable
    servers — most visibly on HEAD requests.
  - Opt-in; the default chain is byte-for-byte unchanged. HTTP/2 still
    rides the SSRF-guarded dialer and the TLS-1.2 fallback transport.
- **`safehttp.NegotiatedProtocol(resp)` — nil-safe ALPN accessor** —
  returns `resp.TLS.NegotiatedProtocol` (e.g. `"h2"` / `"http/1.1"`) or
  `""` for plain-HTTP / nil / errored responses.
  - `WithForceHTTP2` + `NegotiatedProtocol` is the fleet-canonical
    replacement for the per-repo dedicated TCP/443 ALPN-probe handshake
    (e.g. `go_domain_http3_quic_detector`'s `probeALPN`).
  - 6 new unit tests, including an `httptest` server with `EnableHTTP2`
    asserting `h2` is negotiated on HEAD with the option and empty
    without it.

## v0.38.0 — 2026-05-26

### Added

- **`middleware.Logging` — `LOG_SKIP_PATHS` support** — comma-separated list
  of path prefixes whose `request_completed` log entry is demoted from INFO
  to DEBUG, eliminating health-check noise without losing the events.
  - Set e.g. `LOG_SKIP_PATHS=/health,/version,/metrics` in `.env`
  - Prefix matching: `/health` matches `/health`, `/health/live`, etc.
  - Unset or empty → all requests logged at INFO (fully backward-compatible)
  - Parsed once on first request via `sync.Once`; no hot-path allocation
  - 3 new unit tests: skip path → DEBUG, non-skip path → INFO, unset → INFO

## v0.37.0 — 2026-05-26

### Added

- **`proxysupplier` package** — fleet-canonical egress-proxy supplier
  factory. Single source of truth for which upstream proxy to use; adding
  a new provider means one `case` here + `fleet-runner update-dep`, not
  edits across every consumer repo.
  - `Supplier` interface: `Name() string`, `ProxyURL() string`
  - `Config` struct + `EnvConfig()` (reads canonical fleet env vars)
  - `New()` — convenience one-liner for env-driven selection
  - `NewFromConfig(Config)` — explicit factory for struct-config services
  - `HTTPClient(Supplier, time.Duration) *http.Client` — returns nil for
    the "none" supplier (caller falls back to `safehttp`)
  - Supported suppliers: `"plain_proxies"` (PROXY\_HOST/PORT/USER/PASS),
    `"env"` (EXTERNAL\_PROXY\_URL → PROXY\_HOST/PORT fallback), `"none"`
  - Self-proxy guard on every supplier: loopback literals + own hostname +
    DNS resolution; falls back to `noneSupplier` if triggered
  - 8 unit tests

## v0.36.1 — 2026-05-26

### Fixed

- **`apikey.Client` admin calls (`Issue`, `Revoke`, `List`, `Purge`)
  now correctly unwrap the `response.Success` envelope before decoding
  the payload.** All four endpoints in `go-apikey-service` return
  `{"status":"success","data":{...}}` — but `adminCall` was calling
  `json.Decode(body, out)` directly, so the fields nested under `"data"`
  were silently ignored and every struct was returned at its zero value.
  The most visible symptom: `POST /api/admin/keys/issue` returned HTTP 200
  with `{"key":"","user":"","scope":"","note":"","created_at":"","expires_at":""}`
  even though the key was correctly written to the database.
  The fix decodes into a `{"data": json.RawMessage}` envelope first, then
  unmarshals the inner payload — fixing all four admin operations at once.

## v0.36.0 — 2026-05-21

### Fixed

- **`safehttp.NewClient` now honors the `FLEET_REQUIRE_PROXY=1`
  environment variable as a fleet-wide proxy-egress enforcement
  switch.** When the env var is set to a truthy value
  (`1` / `true` / `yes`, case-insensitive, whitespace-trimmed) and
  the caller did NOT explicitly opt out with `WithoutProxy()`,
  `NewClient` behaves as if `RequireProxy()` was passed — panicking
  at startup if no `HTTPS_PROXY` / `HTTP_PROXY` env var is set.

  This closes the silent-fallback hole behind the **2026-05-21
  Hetzner abuse complaint**, where five fleet tools
  (`go_security_headers`, `go_social_graph`, `go_revenue_model`,
  `go_cookie_checker`, `go-url-categorizer-enrichment`) leaked the
  dockerhost IP `176.9.123.221` against external bug-bounty
  targets. All five had `proxy_egress: true` set in `service.yaml`
  and were calling `safehttp.NewClient()` (without
  `RequireProxy()`), so when `/opt/_shared/proxy.env` failed to
  mount or `HTTPS_PROXY` was otherwise unset at container start,
  `http.ProxyFromEnvironment` silently fell back to direct egress.
  The abuse log showed every UA string matching our own tools
  hitting external sites from the gateway IP.

  Paired with `go_fleet_runner ≥ <pending>`, which renders
  `FLEET_REQUIRE_PROXY=1` into the per-service `.env` for every
  service with `proxy_egress: true`. Together they convert silent
  direct-egress into a loud startup panic that the deploy smoke
  gate catches.

  `WithoutProxy()` still wins so SSRF probers, smuggling tests,
  and port scanners that legitimately need direct egress (and pass
  `WithoutProxy()` explicitly) are unaffected.

  No API surface change. Callers that don't have
  `FLEET_REQUIRE_PROXY` in their env continue to behave identically
  to v0.35.0 — fully backwards-compatible.

## v0.35.0 — 2026-05-21

### Fixed

- **`safehttp.WithUserAgent` now actually overrides the outbound
  User-Agent header.** Pre-fix, the option stored the string but
  only the redirect-follow path (`Client.CheckRedirect`) ever
  applied it — every INITIAL request from every fleet caller went
  out with Go's default `Go-http-client/1.1`. Upstreams that gate
  on UA (Wikidata WDQS T400119 was the canary; many CDNs and
  GitHub APIs do the same) silently 403'd against us. This is a
  fleet-wide behavioral fix: every Go service that calls
  `safehttp.NewClient(safehttp.WithUserAgent(ua.Build(slug, ver)))`
  will, after `go get -u`, start sending the configured UA on
  every outbound request instead of the Go default.

  The fix wraps a UA-injecting `http.RoundTripper` into the
  transport chain inside `NewClient`, layered AFTER the SSRF guard
  and the extras/observer transports so the new wrapper does not
  alter SSRF semantics. Per-request UA override is preserved: if a
  caller does `req.Header.Set("User-Agent", "...")` before the
  call, that value wins (the wrapper only injects when the header
  is unset). The injected UA also survives 302 redirect follows.

  No API surface change. Callers that were not passing
  `WithUserAgent` continue to send Go's default UA — backwards-
  compatible for opt-out callers.
