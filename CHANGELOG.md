# Changelog

All notable changes to `github.com/baditaflorin/go-common` are recorded here.
Versioning follows semver on the git-tag axis; the package itself has no
embedded version string (consumers pin via `go.mod`).

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
