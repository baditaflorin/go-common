# Changelog

All notable changes to `github.com/baditaflorin/go-common` are recorded here.
Versioning follows semver on the git-tag axis; the package itself has no
embedded version string (consumers pin via `go.mod`).

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
