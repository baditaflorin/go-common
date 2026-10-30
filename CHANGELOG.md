# Changelog

## [0.112.0] - 2026-10-31

A key delivered as a file follows that file: a renewed key written over it is used without a restart. Groundwork for giving every keystore key an expiry (fleet-state `docs/architecture/ephemeral-credentials`, evidence-key-lifecycle.md 5.3 and 4.3); nothing renews keys yet, and nothing changes for a service whose keys come from environment variables.

### Added

- `credfile`: a reloading key file. `credfile.Open(name, path, opts)` reads the key at start (a failure there fails as before), then looks at the file at most every 30 s (never more than 60 s) when the key is used or the metrics are scraped, and re-reads it when its inode, size or modification time changed. `Source.Rejected(used)` is the 401 path: one immediate re-read (at most one per 5 s per file) and "changed" only if the file now holds another key, so a caller retries once and never loops. A file that goes missing or turns unusable (empty, oversized, world-readable, a value `Validate` refuses) keeps the last good key and counts the failure and every use of the stale key. A non-secret `<path>.meta` sidecar (`credfile.Meta`: `expires_at`, `key_id`, `issued_at`, `renew_after`, `successor_of`, `key_sha256`) gives the key's expiry; a sidecar whose `key_sha256` belongs to another key is ignored, and counted only if it outlives one check interval, so the two renames of a renewal are not an error. The value is never in an error, a log line, `%v` of a Source, `Stats` or a metric. Stdlib only.
- `promx`: `NewCredfileCollectors`, wired by `AutoWire`, exports `fleet_credential_expiry_timestamp_seconds{credential}`, `fleet_credential_reload_total{credential,result}`, `fleet_credential_reload_errors_total{credential,reason}` and `fleet_credential_stale_use_total{credential}` for the credentials a process loaded from files (none for env-var keys). The credential label is bounded to 16 names plus `_other`; budgets are in docs/observability.md.
- `apikey.ResolveCriticalFile(slug, fileEnv, envVars...)` and `MustResolveCriticalFile`: `ResolveCritical` for a key delivered as a file (`FLEET_API_KEY_FILE` by convention). It returns a `*CriticalKey` whose `Value()` follows the file and whose `Rejected` is the 401 path. When `fileEnv` is set the file wins over the env chain (as `GRAPH_API_KEY_FILE` does), so a renewed file is never shadowed by a stale env value; when it is not set, the env chain behaves exactly as `ResolveCritical`. Services opt in; `ResolveCritical` itself is unchanged.
- `FLEET_CREDFILE_RELOAD`: `off` (also `false`, `0`, `no`, `disabled`) makes every file-backed key be read once at start, as before this release.
- `fleetfetch.APIKeyCredential` and `secrets.APIKeyCredential`: the credential names in the metrics (`fleet_fetch_cache_api_key`, `fleet_secrets_api_key`).

### Changed

- **BREAKING (behavioural):** a key read from `FLEET_FETCH_CACHE_API_KEY_FILE` (`fleetfetch.NewClient`) or `FLEET_SECRETS_API_KEY_FILE` (`secrets.NewFromEnv`, `telemetry.Init`) is no longer read once at start: each request sends the file's current key, a renewed file is picked up within a minute, and a 401 re-reads the file once and retries once with the new key if there is one. `fleetfetch`'s auth circuit (open for the life of the client after a 401/403) closes again when the key file holds another key. The start-time read, its checks and its error texts are unchanged, and a key from `WithAPIKey`, `FLEET_FETCH_CACHE_API_KEY`, `FLEET_SECRETS_API_KEY` or `FLEET_API_KEY` behaves exactly as before. Why: keystore keys are getting an expiry, and a key read once dies with it; renewal writes a new file. Verified with tests for an atomic replace, a 401 with and without a renewed key (one retry, never a loop), a missing file (last good key, error and stale-use counts), the off-switch, and the env-var paths unchanged; and by building and testing three fleet services against this tree.
  - summary: `FLEET_FETCH_CACHE_API_KEY_FILE` and `FLEET_SECRETS_API_KEY_FILE` keys are re-read when the file changes and once on a 401; env-var keys are unchanged.
  - off-switch: `FLEET_CREDFILE_RELOAD=off` in the service's environment, no release needed
- **BREAKING (behavioural):** the remediation in `apikey.ResolveCritical` errors (`fix=`) is `fleet-runner key provision <slug>` (an expiring key, installed and the service restarted) instead of `fleet-runner key issue <slug> --never-expires`, which minted keys that never expire. The rest of the error line (`slug=`, `env=`, `reason=`, `docs=`) is unchanged.
  - summary: `ResolveCritical` errors name `fleet-runner key provision <slug>` as the fix instead of `key issue --never-expires`; a log parser keyed on the old text needs updating.

## [0.111.6] - 2026-10-29

A graph writer sends up to 2000 events per request instead of 500, and never more than about 600 KB.

### Changed

- `graph`: `GRAPH_FLUSH_BATCH` defaults to 2000 (was 500). A request is also cut at about 600 KB (`maxBatchBytes`): what does not fit goes out in the next request of the same flush, in order, and a failed request keeps its events for the next flush as before.
- Requires go-fleet-graph 0.4.44, live since 2026-10-29: an older collector answers 400 to a batch over 500 and the sender drops a refused batch. Set `GRAPH_FLUSH_BATCH=500` for a service that talks to an older collector.

Why: with 0.111.4's 30 s interval a busy writer (go-infrastructure-fetch-cache, about 42 events a second) holds about 1,270 events per flush but sent them as three requests of 500, so its request count fell only 19 % per 1,000 events. A batch of 2000 is about 340 KB at the measured 170 bytes an event, well under the gateway's 1 MB body limit (a larger one is answered 413 and dropped, which is what the byte bound prevents when paths are long). The collector commits a request as one group transaction of at most 2000 events, so the write cost per request does not grow.

## [0.111.5] - 2026-10-29

The access log says which address a request really came from, and which host a graph batch came from.

### Added

- `middleware.Logging`: `request_completed` gets `client_ip` (the gateway's `X-Real-IP`) and `node` (a graph writer's `X-Fleet-Node`) when the request carries them. `client_ip` is logged only when the connection's own peer is a loopback, private or link-local address, so a client that reaches a service directly cannot choose it; `node` only when it is a lowercase DNS label. `ip` is unchanged.

Why: behind the gateway `ip` is always the gateway (the collector's log showed 10.10.10.10, the nginx proxy VM, for every one of 41 batches a second, whatever server sent it). The nginx templates already forward `X-Real-IP`; the logs just never used it. Both fields are for reading logs, never for deciding who may do what.

## [0.111.4] - 2026-10-29

Graph writers flush every 30 seconds instead of every 10, and no two writers keep the same rhythm.

### Changed

- `graph`: `GRAPH_FLUSH_INTERVAL` defaults to 30 (was 10). Each wait is `interval * (1 +/- 10 %)`, drawn again after every flush, so writers that restarted together in a fleet deploy drift apart. Setting the variable still wins.

Why: measured on the live collector over one hour (2026-10-28): 148,752 `POST /events` (41 a second) from 556 writers, carrying 2.62 million events, 17.6 per request. The collector checks the writer's key against the keystore on every request (writes are deliberately not cached, so a revoked key stops working on the next batch), so each request costs a keystore call, a log line and a queue slot. A quiet service sent three requests with a handful of events where one would do. At 30 s a busy writer still fills batches (the ring holds 10,000 events, the busiest writer produces about 70 a second, so about 2,100 per interval), and a quiet one sends one. Expect about a third of the requests; the events and their timestamps are unchanged, and the graph lags by at most half a minute more.

## [0.111.3] - 2026-10-28

The Apache License 2.0 now also covers every earlier release of go-common. Until 0.111.1 the repository carried no license file, so v0.111.0 and the versions before it were published without one.

### Changed

- `NOTICE` and `README.md` state that the copyright holder licenses all earlier releases, and every later one, under the Apache License 2.0. The tags themselves cannot change, so a version older than v0.111.1 still has no LICENSE file inside it; this statement is the grant.

Why: a module that was licensed only from one release on leaves everyone who already depends on an older version without a right to use it, and pkg.go.dev decides what to show by the files inside each tagged version.

## [0.111.2] - 2026-10-28

Two opt-out build tags that keep the SPIFFE Workload API client and the Pyroscope client out of a `server.New` binary, a CI check that holds them there, and `docs/footprint.md`, which says what a service pays for importing go-common. A binary built without the tags does what it did and is the same size to the byte; nothing changes for any service until it adds a tag to its own build.

### Added

- Build tag `gocommon_nospiffe`: `server` does not import `spiffe`, so the binary links neither go-spiffe nor go-jose. `graph`'s SPIFFE path (`GRAPH_SPIFFE_COLLECTOR_URL`) then has no Workload API client: the factory `server.New` installs returns an error that names the tag, which graph's sender logs on its first attempt and every tenth after it, and the sender keeps using the writer key as it does while the Workload API is unreachable. Without the variable the factory is never called.
- Build tag `gocommon_nopyroscope`: `server` does not import `profiling`, so the binary does not link the Pyroscope client. With `PYROSCOPE_SERVER_ADDRESS` set it logs one line saying the binary was built without it and sends no profile.
- `scripts/linkcheck.sh`, `scripts/linkcheck.txt` (one line per tag), `scripts/linkcheck-selftest.sh` and the reference service `scripts/linkcheck/service`: with each tag the feature's packages are absent from `go list -deps` of the reference service, and without it they are present, so a refactor cannot turn a tag into a no-op or make a default build lose a feature. `scripts/linkcheck.sh --size` also builds both and prints the stripped sizes.
- `docs/footprint.md`: the measured cost of a `server.New` service against plain `net/http` (binary, modules, idle RSS split into private and file-backed, start-up, initialisation, goroutines, per request), what each feature costs, and why nested modules or separate repositories would not change it.

### Changed

- `server`: the code that imports `spiffe` (`graphMTLSClient`) and `profiling` (`startProfiling`, `startServerProfiling`) moved to files behind `//go:build !gocommon_nospiffe` and `!gocommon_nopyroscope`, with a counterpart behind each tag; the two tests that need the real packages moved with them. No behaviour change without a tag.
- CI: the `validate` step runs `scripts/linkcheck-selftest.sh` and `scripts/linkcheck.sh`, and builds, vets and tests `server` with both tags.

Why: the question was whether go-common makes every service heavy and whether it should be split into modules. A `server.New` service is 20.4 MB against 5.8 MB for plain `net/http`, but 16.6 of its 19.6 MB idle RSS are the executable's own pages, mapped clean from the page cache (2.6 MB is private), and go-common's own code is 0.4 MB of it. The rest is what the service kit links (OpenTelemetry with gRPC and protobuf, Prometheus, go-spiffe, Pyroscope, the MCP SDK). A binary links packages, not modules, so nested modules or separate repositories would not change what it links; what changes it is not importing a package. SPIFFE and Pyroscope are the two features no service in the scanned repositories configures and every `server.New` binary links, so they get a tag. The MCP SDK (`WithMCP`, used by 129 repositories) and the OTLP exporter are left alone; `docs/footprint.md` says what dropping them would save.

Cost: none for a binary built without a tag (20,361,378 bytes before and after to the byte for the minimal service, and 20,369,570 for one with `client`, `selftest`, `metrics` and the rest of the usual import set; idle RSS the same within the spread). With both tags, the minimal service goes from 20,361,378 to 19,083,426 bytes (-1.28 MB, -6.3%), from 43 linked modules to 38 and from 503 packages to 468; idle RSS from 19.24 to 17.95 MB (-1.29 MB, -6.7%, of which 0.1 MB private and the rest the executable's pages), and the same start-up time, initialisation and goroutine counts within the run-to-run spread (12 runs each, golang:1.26.10, 3 cores, medians; `docs/footprint.md` has the method).

## [0.111.1] - 2026-10-28

go-common is licensed under the Apache License 2.0 from this release on. It was public without a license file, which leaves everyone but its author without the right to use it.

### Added

- `LICENSE`: the Apache License 2.0, the text the other open-source repositories of this account carry.
- `NOTICE`: the copyright line and the attribution sentence that section 4(d) of the license makes redistributors keep. Anyone may use go-common commercially; the license and the NOTICE text have to travel with it.
- `README.md`: a License section.

Why: the open-source services built on go-common (the agent-history family first) cannot be adopted by anyone else while a dependency has no license, and pkg.go.dev shows a module's documentation only when its tagged version contains a recognised license.


## [0.111.0] - 2026-10-25

Every response a `server.New` service sends names the service and its release in two headers, `X-Service-Name` and `X-Service-Version`, so a caller can record which release produced an answer when the body names none.

### Added

- `server`: `X-Service-Name` (`Config.AppName`, what `/health` reports as `service`) and `X-Service-Version` (`Config.Version`, what `/version` reports) on every response `Server.Handler` sends, so on everything `Start`, `Run` and `RunTier` serve: a handler's answers, success or error (a `meshresult.WriteOutcome` envelope, `http.Error`, an empty 503), the keystore gate's 401 and 403, the mux's 404, 405 and redirect, the 400 for a repeated `source` parameter, `/health`, `/version`, `/readyz`, `/metrics`, `/metrics/json`, `/capabilities`, `/schema`, `/selftest` (the default and a registered suite, 200 or 503), HEAD requests and streamed responses. It is the outermost layer of the handler, so nothing inside it can answer without the headers. A value that is empty, longer than 256 bytes or holds a control character is not sent. Nothing secret: both values are already public at `/health` and `/version`, which the keystore gate never closes.
- `header.ServiceName` (`X-Service-Name`) and `header.ServiceVersion` (`X-Service-Version`).

Why: DomainScope records on every enrichment row the release of the fleet service that produced it, so bad data can be traced to a release and a backfill can re-run only the rows of old releases. Many answers name no release in their body (analyze-headers answers `{"status","data"}`; no `meshresult.WriteOutcome` envelope has a version; plain-text errors have none), so those rows have a NULL version. DomainScope's client reads these two headers when the body names none (go-url-categorizer-api PR #2945, deployed); this is the producer side, and it reaches a service when the service bumps go-common and is redeployed.

### Changed

- **BREAKING (behavioural):** the server's values win over a handler's. They are set before the handler runs and set again when the response starts (the first `WriteHeader` of 200 or above, `Write` or `Flush`) or when the handler returns without writing, so a value a handler set, added, deleted or overwrote in place is replaced, and a 1xx informational response does not end that. Why: the headers name the process that wrote the response. A service that relays another answer must not pass on that answer's identity, and a website must not be able to claim one through a relay. The fleet fetch cache is the case in point: it runs `server.New` and adds every header it stored from the service it fetched (`writeEntry`), so its answers carry the cache's own pair, never the stored service's (DomainScope already ignores the headers on answers the cache served, for this reason). A relay that wants to say who produced the body says it in a header of its own. No repository in the organisation sets either header today (code search, 2026-10-25), so no service's answers change beyond the two added headers. The writer forwards `Flush` and has `FlushError` and `Unwrap`, like the middleware writers, so streaming and `http.ResponseController` (deadlines, `Hijack`) work through it.
  - summary: every `server.New` response carries `X-Service-Name` (AppName) and `X-Service-Version` (Version); a handler's own values for these two headers are replaced.

Cost: one allocation (the writer, 96 bytes) and about 0.16 to 0.19 us per response (`BenchmarkIdentityMiddleware`, new). `BenchmarkServerRequest` (the whole default stack) went from 78 to 77 allocs/op and from 5,875 to 5,947 B/op on the same build host (golang:1.26, 2000 iterations, every run identical); the one allocation fewer was not traced.

Verified on a build host (0mcp_docker_build) in golang:1.26 containers: gofmt, `go vet ./...`, `go test -count=1 ./...` (76 packages), `go build ./...`, `go test -race` of server, header, middleware, reqstats and telemetry, `pkgindex -check`, `envdoc -check`, the dependency budget and its doc, `whatchanged -gen` and `-check`; `release-check`, `apidiff-check`, `bench-gate`, the full `-race` run and the fuzz step in CI. The tests send every response class above through `httptest.NewServer(srv.Handler())` and check that each carries exactly one value of each header; a handler that sets, adds, deletes or overwrites them in place (writing or not); a fetch-cache server that replays a stored `X-Service-*` pair the way `writeEntry` does; unusable values (empty, CR LF, DEL, over 256 bytes; none usable leaves the handler unwrapped); a streamed answer through `http.Flusher` and through `http.ResponseController`; 103 Early Hints then 200; `SetWriteDeadline` and `Hijack` through `http.ResponseController`; and at most one allocation per response.

## [0.110.0] - 2026-10-24

`botwall`: the one copy of the bot-protection challenge-page detector that go-js-proxy and the fleet fetch cache each carried, so a new vendor rule or a precision fix is made once.

### Added

- `botwall.Detect(html) string`: the vendor of a challenge page (`akamai`, `aws_waf`, `cloudflare`, `datadome`, `google_sorry`, `imperva`, `perimeterx`, `sucuri`) or `""`. `botwall.Vendors()` lists those values (sorted, a copy) for a service that pre-registers a per-vendor metric label. `MaxBody` (160 KiB) and `MaxVisibleText` (3,000 non-space bytes) are the two size limits: a document over either is never a challenge page. Vendor-specific marker pairs only, never a generic "captcha" or "access denied"; the Imperva and AWS WAF rules need the interstitial's own shape, because both vendors inject a script into every page of a protected site (real 404 pages of abbeyroad.com, abelandcole.co.uk and a16zcrypto.com were flagged by the vendor's injected script or SDK name alone before go-js-proxy 0.12.2 and fetch cache 0.3.49).

Why: the detector was written twice, identically, in go-js-proxy 0.12.0 and fetch cache 0.3.48, and both copies had to change together for the Imperva / AWS WAF precision fix. The two services adopt this package in their next releases; the code is moved, not rewritten, and the services' own tests keep their end-to-end cases.

Verified: `go vet ./botwall/`, `go test -race -cover ./botwall/` (98.3%). The tests are the ported ones from both services (every vendor fixture, the ordinary-page and real-404 negative cases, `visibleTextLen`) plus the exact size and visible-text boundaries, the rule that each vendor needs its own marker pair, `Vendors()` being exactly what `Detect` can return, and `FuzzDetect` (25 s locally, 761,327 executions: terminates, only a listed vendor or `""`, `""` over `MaxBody`, deterministic). A mutation check (10 loosened rules: script alone enough for Imperva, AWS and Cloudflare, host alone for DataDome, title alone for Akamai, name alone for Sucuri, any "captcha" for PerimeterX, both caps off by one, script bodies counted as text) is caught by the tests; the first run left the DataDome and Akamai ones alive and the negative cases above were added for them.

## [0.109.1] - 2026-10-21

`build_info` carries the go-common version the binary was linked with, as a new label, so a dashboard or an alert can tie a running service to the go-common it runs.

### Added

- `build_info{go_common_version}`: the version of `github.com/baditaflorin/go-common` in the binary's module list (`debug.ReadBuildInfo`): `v0.109.0` for a release, a pseudo-version for a build from a commit, `devel` when go-common is the main module (its own tests) or a `replace` points at a directory, `_unknown` when the binary carries no module information. It joins the five labels `build_info` already had (`service`, `version`, `git_sha`, `built_at`, `go_version`); there is still one series per process, constant for its life. A query that names the labels it uses is unchanged; a service's series is replaced once, at its restart, as at every release (`version` and `git_sha` are labels too). The series budget in `docs/observability.md` names the new label.

Why: after a fleet-wide bump the running services could be listed with their Go toolchain (`go_version`) and their own `version`, but not with the go-common they run, so a failing selftest could not be tied to a go-common version except by comparing process start times with merge times by hand. The label is read from the binary itself, so it is what runs, not what `main` says.

Verified: `go vet ./promx/` and `go test ./promx/... ./testhelpers/promxtest/...` in a `golang:1.26` container, including `TestSeriesBudgets` and `TestCollectorsAreBounded` (one series, six labels); `TestGoCommonVersionOf` covers a release, a pseudo-version, a replace by a module with a version and by a directory, go-common as the main module (with and without a tag), a module that is not linked, and a similar module path; `TestBuildInfoHasTheGoCommonVersion` reads the gathered series.

## [0.109.0] - 2026-10-21

`graph` can send its event batches to the collector over SPIFFE mTLS, proving the workload's X.509-SVID instead of sending the writer key. It is off unless a service sets the new variables, and the writer key stays the fallback.

### Added

- `graph`: the SPIFFE path for event batches. When `GRAPH_SPIFFE_COLLECTOR_URL` (the HTTPS root of the collector's private mTLS listener, e.g. `https://go-fleet-graph:8443`), `GRAPH_SPIFFE_COLLECTOR_ID` (the exact SPIFFE ID that listener must prove) and `SPIFFE_ENDPOINT_SOCKET` (set by the fleet's Compose SPIFFE overlay) are all set, the sender POSTs each batch there with no `X-API-Key`, authenticated by the workload's SVID. Why: the collector's writer keys are one per service, each with its own renewal timer, and about 100 services hold one; a workload identity the fleet already issues can replace them. The first consumer is agent-tools → fleet-graph.
  - **Fallback:** a configured writer key keeps working. A batch whose mTLS send fails on the path (no answer, a refused handshake, `401`, `404`, `405` or `421`) is sent once more with the key in the same flush, and the next minute of batches use the key before the SPIFFE path is tried again. An answer from the collector itself is final for that batch, as on the key path: `2xx` delivered, `400`/`403`/`413`/`415`/`422` dropped as rejected, `408`/`429`/`5xx` kept for the next flush. So a slow or overloaded collector is not sent every batch twice. A `403` also starts the minute on the key, because it can mean the listener maps this workload to another writer.
  - **Without a key:** the SPIFFE path alone enables emission (`GRAPH_ENABLED=true` plus the three variables), and a batch the path fails to deliver stays in the sender for a later flush.
  - **Until the first SVID:** the sender connects in the background, waiting up to 30 s per attempt and pausing 5 s, then doubling to 5 min, between attempts; it uses the key meanwhile. It logs one line when the path is ready, the first failed attempt and every tenth, and one line per minute of fallback. No SVID, key or Workload API response is logged.
- `graph.MTLSClient`, `graph.MTLSClientFactory` and `graph.SetMTLSClientFactory`. `graph` does not import the Workload API (that would add go-spiffe and go-jose to `safehttp`'s dependency budget, `scripts/dep-budget.txt`); it uses the client a factory returns. The first factory installed wins.
- `server.New` installs the factory, built on `spiffe.NewHTTPClientWithTimeout` (redirects off, no proxy, the collector ID as the only allowed server). A service on `server.New` therefore needs only the environment; any other service calls `graph.SetMTLSClientFactory` itself. `server` now reaches `github.com/spiffe/go-spiffe/v2` and `github.com/go-jose/go-jose/v4` (`docs/dependencies.md`); `google.golang.org/grpc`, which the Workload API client uses, was already linked through `telemetry`. A service that bumps gets the two modules added to its `go.mod` as indirect requirements.
- `graph.Counters.BatchesSentSPIFFE` (the part of `BatchesSent` delivered over the SPIFFE path) and `BatchesSPIFFEFallback` (batches the SPIFFE path failed that went with the key), exported by `promx.GraphCollectors` as `fleet_graph_batches_sent_spiffe_total` and `fleet_graph_batches_spiffe_fallback_total`. A rising fallback count means the mTLS path is broken and the key carries the traffic.
- The sender now reads the collector's short answer (at most 4 KiB) before closing it, on both paths, so the connection is reused; on the SPIFFE path a new connection is a new mTLS handshake.
- Tests: the URL and ID checks; the path needs all three settings; batches go keyless over the SPIFFE path; each path failure (`401`, `404`, `405`, `421`, a transport error) falls back to the key and starts the minute, after which the SPIFFE path is tried again; each collector answer (`503`, `500`, `429`, `400`, `413`, `403`) is final and never reaches the key; a key-less sender keeps the batch; a key cool-down does not stop the SPIFFE path; a factory installed after the sender started is used (socket, ID and timeout checked), a second factory is ignored, and `Shutdown` closes the client; a failed connect is retried; the connector stops with the sender; `server`'s factory returns a nil client with the error.

Not changed: with none of the new variables set, nothing behaves differently; the key path, its `401`/`403` cool-down and `GRAPH_COLLECTOR_URL`'s canonical-host rule are as before.

Verified on a build host in golang:1.26 containers: gofmt, `go vet ./...`, `go test -count=1 ./...` (all packages), `go build ./...`, `pkgindex -check`, `envdoc -check`, the dependency budget and its doc, `whatchanged -check`, `release-check` (0.109.0 is free), `-race` on graph, promx, server and spiffe, and apidiff (no incompatible change).

## [0.108.1] - 2026-10-20

Release bookkeeping, no package behaves differently: the security floor now covers the 0.102.70 fixes, 0.107.0's gate fix has its floor decision and a `**SECURITY:**` bullet, and the 0.105.2 entry says where the 0.105.1 text is.

### Changed

- `compat/compat.json`: `min_safe` is `v0.102.70` (it was `v0.102.2`), `min_safe_date` is that release's commit time, and `reasons` gets its fixes: `safehttp.IsBlocked` also refuses a NAT64 or 6to4 address that carries a blocked IPv4 address, and `jsbundle`'s default client, which had no timeout and no SSRF guard, is guarded. 0.102.70 was never tagged; the first tagged release that has it is `v0.103.0`. Every module in the fleet is on v0.104.0 or newer (audited 2026-10-20) and no repository in the organisation references `compat.CheckGoMod` or `compatcheck` (searched 2026-10-20), so nothing starts to fail; a pin below the floor is now reported as missing those fixes, which is what the floor is for.
- `compat/compat.json`: `min_safe_unchanged` records `v0.107.0` with its reason: the gate bypass was latent (an opt-in option, and `http.ServeMux` redirects the paths), so the floor stays at `v0.102.70` and does not report every service between it and 0.107.0. It can move to `v0.107.0` once the fleet is on it.
- `CHANGELOG.md`: the 0.107.0 entry gets the `**SECURITY:**` bullet for the fix it already describes, and the 0.105.2 entry, which holds two changes under one heading, says where the 0.105.1 text starts.

Why: `0.102.70` was the one security fix with no floor decision (0.105.5 added the rule and left it undecided), the fleet is all above it so raising the floor costs no service anything, and 0.107.0 is the first fix the new rule applies to.

## [0.108.0] - 2026-10-20

`fleetfetch` clients now keep at most 32 MiB of a response body unless told otherwise, instead of reading whatever size the origin sends.

### Changed

- **BREAKING (behavioural):** a `fleetfetch.Client` (and `NewHTTPClient`, and the safehttp fetch-cache delegate `server.New` installs when `FLEET_FETCH_CACHE_URL` is set) cuts a response body at `DefaultMaxBodyBytes` = 32 MiB when it sets no limit, on the cache path and on the direct fallback path. It read the whole body before: `maxBodyBytes` was 0, "unlimited, preserving the historical behavior", and about 118 of about 169 `fleetfetch.NewClient(` call sites in the fleet workspace never set one, so one multi-gigabyte response from a page a stranger chose could take all of a worker's memory. A body over the limit is no error: `Response.Body` holds the first 32 MiB and `Response.Truncated` is true (`NewHTTPClient` sets the response header `X-FetchCache-Truncated: true`). A caller that needs bigger bodies sets `WithMaxBodyBytes(n)`; one that really wants them whole sets the new `WithUnboundedBody()`. Off-switch for a service you cannot change: `FLEET_FETCH_MAX_BODY_BYTES` in its environment, a number of bytes, or `0` for no limit at all (discouraged); an option in code beats the variable. Where 32 MiB comes from: it is the `client.DefaultBodyCap` this module already applies to fetch-proxy responses; it is above every explicit `WithMaxBodyBytes` limit in the fleet workspace (about 50 call sites in about 30 workspace directories, read 2026-10-20: 1 to 5 MiB almost everywhere, 8 MiB in three, 12 MiB for one image fetcher) and above `jsbundle.DefaultMaxScriptBytes` (8 MiB); and the sitemap protocol allows 50 MiB uncompressed, so a silent cut at 8 MiB would break sitemap and large-feed fetchers that do not read `Truncated` (one that needs the full 50 MiB sets `WithMaxBodyBytes(50<<20)`). This is a census of limits services chose, not of observed body sizes: no body-size histogram exists (`safehttp_egress_response_bytes_total` is a counter). The limit is a named constant so the number is one edit.
  - summary: A fleetfetch client (and the server.New safehttp cache delegate) with no limit now cuts response bodies at 32 MiB and sets Response.Truncated; they were read whole before.
  - off-switch: FLEET_FETCH_MAX_BODY_BYTES=<bytes>, or 0 for no limit (discouraged), in the environment; WithMaxBodyBytes(n) or WithUnboundedBody() in code
- **BREAKING (behavioural):** `WithMaxBodyBytes(n)` with `n <= 0` no longer means "unlimited": it is ignored and the limit stays the default. Unlimited is `WithUnboundedBody()` only, so that nobody gets it by passing an unset configuration value. The call sites in the fleet workspace pass positive values (`maxBodyBytes`, `maxBodyBytes+1`, `2<<20`, ...), so none changes.
  - summary: WithMaxBodyBytes(n <= 0) no longer means unlimited: it is ignored and the 32 MiB default stays. Unlimited is WithUnboundedBody() only.
  - off-switch: WithUnboundedBody() in code, or FLEET_FETCH_MAX_BODY_BYTES=0 in the environment
- The safehttp delegate in `server` (`fetchCacheDelegate`) treats a truncated response as a miss: the request falls through to safehttp's direct path, as for any cache error, and the caller gets the whole body. Without this a transparent cache would have handed every safehttp user a silently cut body above 32 MiB. Archive-only (Common Crawl) requests get the same 502 as any archive failure, never a partial capture. The cost is a second origin request for a response over the limit.
- `apikey.Client`'s admin calls put at most 4 KiB of an error response (HTTP 4xx other than 401) into the returned error, marked `[truncated]` when cut; the whole body was read before.

### Added

- `fleetfetch.DefaultMaxBodyBytes`, `EnvMaxBodyBytes`, `WithUnboundedBody`, `Response.Truncated`, `Stats.Truncated` and `Event.Truncated` (true on the one event of a fetch whose response was cut, for observers; `promx` records it as `fleet_fetch_truncated_total{service}`, with no host label). The variable is declared in the `env` registry, so it is in `docs/ENV.md` and an unparsable value is reported at start-up. The first truncation in a process logs one line naming the host and the ways to raise the limit.
- Tests: the default, the environment override (empty, number, 0, negative, text) and option precedence; truncation, the flag and the exact-limit case on both the cache path and the direct fallback path, at the real 32 MiB default with a body of 32 MiB + 1 and of 32 MiB; the opt-out on both paths; the `limit+1` idiom 23 call sites use; that `readBody` reads one byte past the limit and no more from an endless body; the counter and the event; the `NewHTTPClient` header; the delegate falling through to the direct path with the whole body; the apikey error body cut at 4 KiB.
- `promx.FleetFetchCollectors` exports `fleet_fetch_truncated_total{service}`, one series per service: it counts fetches whose response was cut at the body limit. If #190 (series budgets) merges first, its budget list needs one row for it.
- Not changed: `fleetfetch.LoopbackClient` (a test double, "use only in tests") still reads whole bodies, and the success path of `apikey.Client` admin calls still decodes the whole reply.

Verified on a build host in golang containers: validate (including `envdoc -check` and the dependency doc), race, lint (no new findings), vuln, forward-compat (Go 1.27.2), api (additive only: new exported identifiers and struct fields), bench, release and the 5 s fuzz run all pass.

## [0.107.0] - 2026-10-20

The keystore gate's exemption for declared public path prefixes (`PublicPathPrefixes`, new in 0.104.1) now applies only to canonical paths, and `server` checks at start-up that no keystore-gated route lies under a declared prefix.

### Changed

- **BREAKING (behavioural):** `TokenAuthKeystore` exempts a request from authentication, by a declared `PublicPathPrefixes` entry or by the fixed `/health`, `/version`, `/selftest`, `/_gw_health`, `/capabilities`, `/openapi.json`, `/agent.json` list, only if its path is canonical. A request whose path starts with a declared prefix (or equals a fixed path) but is not canonical is answered `400 {"error":"Bad Request","reason":"non-canonical path"}` by the gate, with or without a credential, and is not passed on. Canonical means: no `.` or `..` segment and no `//` (a trailing slash is fine), no encoded dot, slash or backslash (`%2e`, `%2f`, `%5c`, either case) in the raw or the decoded form (so `%252e` is refused too), no backslash, and the raw form starts with the declared prefix as the decoded one does (`/%63/x` is not `/c/x`). Why: the gate decided from the path as written while a router decides from the path as it reads it, and routers differ (`http.ServeMux` redirects dot segments, a router that cleans the path first serves the cleaned one, one that decodes twice reads `%2e%2e` as `..`, one that routes on the raw path keeps `%2f`). Behind a router that cleans the path before routing, `/c/../admin` and three more spellings of it reached a keystore-gated `/admin` with no key when `/c/` was declared; behind `http.ServeMux`, which `server.New` uses, they were redirected or served by the `/c/` handler. 400 and not 401: no credential makes such a path unambiguous, and a 401 would be read as a bad or missing key (see `denyBadPath`). Who is affected: a service that declares `PublicPathPrefixes` (one service at the time of writing) and a caller that sends a non-canonical path under it, e.g. `/c//tok` or `/c/tok/..`; the callbacks the option was added for, `/c/<token>[/<id>]`, are canonical. A fixed path sent with a percent-encoded letter (`/%68ealth`) is refused too. Not affected: services that declare no prefix (the fixed list is unchanged for every canonical request), and any request that matches neither test, which is authenticated exactly as before; `/health/` is still not exempt. The refusal is observed as `AuthSourceBypass` with `AuthResultDeny`, so `apikey_auth_total{source="bypass",result="deny"}` counts it. There is no off-switch, deliberately: an environment variable that restores matching on the raw path would restore the bypass behind a router that cleans paths, and the gate cannot know which router it is in front of. A service that needs a non-canonical path accepted should say which form, so the rule can allow that form and nothing else; to go back, stay on 0.105.0.
  - summary: A request under a declared PublicPathPrefixes entry, or for a fixed probe path, with a non-canonical path (dot segments, //, encoded . / \) now gets 400 from the gate instead of skipping auth.
- **SECURITY:** a declared public path prefix (`PublicPathPrefixes`, 0.104.1 to 0.106.0) was matched against the path as written, while a router that cleans paths before routing chooses the handler from the cleaned path, so behind such a router a request like `/c/../admin` skipped authentication and reached a gated handler. `http.ServeMux`, which `server.New` uses, redirects those paths instead, so the hole was latent: the only service known to declare a prefix (it is listed in the fleet registry) routes on `Server.Mux`. 0.107.0 closes it (the canonical-path rule in the bullet above); the floor decision is in `compat/compat.json`: `min_safe` stays, with the reason.
  - summary: A declared public prefix was matched on the raw path; behind a router that cleans paths first, /prefix/../gated skipped auth (latent behind http.ServeMux). Fixed by the canonical-path rule.

### Added

- `server.Server.Handle`, `HandleFunc`, `HandlePublic`, `HandlePublicFunc`: register a route on `Mux` and record the pattern. `Handle`/`HandleFunc` mean "protected by the keystore gate", `HandlePublic`/`HandlePublicFunc` "meant to be served without a key". The server's own gated routes (`/readyz`, `/schema`, `/metrics/json`, `/mcp`, the cancel route) are recorded the same way.
- `Server.Start` returns an error, and does not listen, when a recorded gated route lies under a prefix declared with `WithKeystoreAuthPublicPrefixes` (`/c/admin`, `GET /c/{token}`, an exact `/c` for the prefix `/c/`, and with a prefix `/c` its siblings `/config` and `/cache/{id}`), naming every route and prefix. Nothing changes for a service that declares no prefix or registers routes only on `Mux`: those are not recorded and not checked. Routes shorter than the prefix (`/`, `/{x}`) are not reported.
- `middleware.MatchPublicPathPrefix(path, prefixes)`: the gate's own prefix test (`""` and `"/"` never match), so a router can check its routes against the same rule.
- Tests: the four paths from the review (`/c/../admin`, `/c/%2e%2e/admin`, `/c/x/../../admin`, `/c/..%2fadmin`) and 18 more spellings against both router shapes, through real HTTP servers, with no key, a bad key, a local key and a gateway header: 400 each, no handler reached, the keystore never asked; `/health`, `/c/`, `/c/tok`, `/c/tok/id` and the other canonical forms unchanged; `/health/` still 401; a table for the canonical rule; a prefix that needs escaping; a zero-allocation test for the canonical case; `FuzzPublicPathBypass`, which states the property (no reading a router may apply takes an exempted path out of the prefix); the route registry (a table of cases, the server's own mounts included), `Start` returning the error, and the prefix end to end through `server.New`'s handler chain.

Verified on a build host in golang containers: validate, race, lint (no new findings), vuln, forward-compat (Go 1.27.2), api (no incompatible change), bench, release and the 5 s fuzz run all pass. Four deliberate breaks of `isCanonicalPath` and `classifyPublicPath` (skip the canonical check, drop the dot-segment test, drop the encoded-separator test, drop the raw/decoded agreement) each fail the tests that name them.

## [0.106.0] - 2026-10-20

Nothing in go-common bounded or tested how many series a metric can create, and two labels taken from requests took the fleet's Prometheus down in three days (`method` in 0.102.74,
`host` on the egress and fleetfetch histograms in 0.105.0). This release adds the tools that bound and test a label (`promx.BoundedLabel`, `testhelpers/promxtest`, a per-metric
series budget in docs/observability.md that a test holds exact) and fixes what they found when run over every collector `promx` has: two panics a request could trigger, and three
labels that grew with the input. Services pick it up with the next go-common bump.

### Added

- `promx.BoundedLabel` (`NewBoundedLabel(limit, WithMinHits(n), WithCandidateWindow(n))`, `Value`, `MaxValues`, `OtherLabel`, `UnknownLabel`, `MaxLabelValueLen`): the one supported way to
  put a value the process does not choose in a label. It is the 0.105.0 host rule factored out of the egress and fleetfetch collectors (which now use it, unchanged: the same 32 heavy
  hitters, admitted on their 64th hit) with a first-come mode (`WithMinHits` 1, the old 256-host and route caps) and the two guards below. The HTTP route and the backoff, backoffcoord and
  ratecoord hosts use it too.
- `testhelpers/promxtest`: `AssertBounded(t, register, probes, maxSeries)` registers a collector set in a fresh registry and drives a hostile corpus through it (`Inputs`: 2,000 distinct
  hosts, methods, paths, schemes, user agents and names, every status from 100 to 999, values over 256 bytes and one of 1 MiB, unicode, control characters, and values that are not valid
  UTF-8), twice, with the input at two sizes. It fails when a probe panics, when the larger run creates more series than the smaller (a label that grows with its input), when the series
  exceed the budget, when a label value is longer than 256 bytes, and when the probes reach nothing. `Measure` and `Report` give the series per metric and the ceiling a metric's labels allow;
  `Run` drives probes into a registry of your own.
- docs/observability.md has a Series budgets table: the most series each `promx` metric can hold in one process under hostile input (82 metrics; 497,212 together, 483,160 of them the three
  HTTP metrics that carry `route`). `TestSeriesBudgets` holds it exact: a metric without a row, a row without a metric, a number that is not the ceiling the probes measure and a labels column
  that is not the metric's labels all fail. `TestCollectorsAreBounded` runs `AssertBounded` for each of the 20 collector sets, and `TestEveryCollectorHasAHarnessCase` fails for a new
  `New...Collectors` constructor that has no hostile probes.
- Tests: `TestHTTPServesARequestWhoseTargetIsNotUTF8` (a real server, `GET /%ff`), `TestEgressObservesAHostThatIsNotUTF8`, `FuzzBoundedLabel`. The harness run against 0.105.0's behaviour (the
  scheme and status mappings and the two guards taken out) fails with the panics in the HTTP, egress and fleetfetch cases and with the HTTP `status` at 900 values.

### Fixed

- **BREAKING (behavioural):** a `route` or `host` label value that is not valid UTF-8, or longer than 256 bytes, is `_other`. client_golang panics in `WithLabelValues` on invalid UTF-8, and
  both are reachable: `GET /%ff` gives the HTTP middleware the path `"/\xff"`, and `http://%ff.example/` gives an egress event the host `"\xff.example"`. The middleware panicked after the
  handler had run (net/http recovered, logged a stack and closed the connection without the reply: one request, before authentication, from anyone who could reach the port); the egress
  observer panicked inside the caller's `RoundTrip` or response-body `Close` once the host had been seen 64 times, which in a worker goroutine ends the process. A path or host of up to 1 MiB
  was also held for the life of the process once admitted (route: first-come, 512 of them) or while a candidate (host: up to 1,024) and written into every scrape. Counts are unchanged: the
  request is counted under `_other`.
  - summary: a `route` or `host` label value that is not valid UTF-8 or is over 256 bytes counts under `_other` (it panicked, or was kept for the life of the process).
- **BREAKING (behavioural):** `scheme` on `safehttp_egress_requests_total` is `http`, `https` or `_other` (`_unknown` when empty); it was the scheme of the URL the caller handed to the client.
  A service that fetches the URLs its users send could be made to mint a series set per invented scheme (5,000 schemes made 5,001 series in the probe). safehttp itself only dials http and https.
  - summary: `scheme` on `safehttp_egress_requests_total` is `http`, `https` or `_other` instead of whatever scheme the URL had.
- **BREAKING (behavioural):** `status` on `http_requests_total` is the code itself when it is a registered one (the 62 codes `http.StatusText` knows) and its class otherwise (`499` is `4xx`,
  `520` is `5xx`; a `status=~"5.."` matcher still selects it), `_other` outside 100-599. A handler that relays an upstream's status writes anything from 100 to 999 (900 values, each a series
  set per method and route). The label is now a table lookup: it no longer allocates per request.
  - summary: `status` on `http_requests_total` folds codes that are not registered into their class (`499` becomes `4xx`) and codes outside 100-599 into `_other`.
- **BREAKING (behavioural):** `fleet_degraded_total` names at most 64 `primitive` and 16 `suffix` values per process, first-come, and counts the rest under `_other`. Both came from whatever string
  a caller passed to `degraded.Sink.Append`, and the degraded package invites a third-party host in it. A service that uses a few names, as the convention says, sees no change.
  - summary: `primitive` and `suffix` on `fleet_degraded_total` take at most 64 and 16 values per process; the rest count under `_other`.

### Findings left alone

- The HTTP collectors cap `route` per label, not per `(method, route)` pair: 514 routes by 10 methods is 5,140 pairs, which the two histograms cost 14 and 12 series each (71,960 and 61,680
  per process, 349,520 for the counter with its 68 statuses). Normal traffic is a few dozen pairs. Capping the pair, or dropping `method` from the histograms as 0.105.0 dropped `host`, is a
  behaviour change for the owner to decide.
- `safehttp_backoff_waited_seconds` and `backoffcoord_wait_seconds` are histograms with `host` at 256 first-come values: 2,838 series each, the largest host-labelled families left. They are
  the 0.105.0 problem at a smaller size (the first 256 hosts after boot); the same heavy-hitter rule would take them to 374. Not changed here: nothing says who reads them by host.
- No lint rule: forbidigo (the pinned golangci-lint) matches `WithLabelValues` calls and cannot see their arguments, so it flags `WithLabelValues("const")` like `WithLabelValues(r.URL.Path)`.
  The budget table and the harness are the check.

## [0.105.5] - 2026-10-20

A security fix has to settle `min_safe` in the same change: `scripts/release-check.sh` now requires it, and the decision is written down. The mechanism only: `min_safe` is not moved here,
and no package behaves differently.

### Added

- A `**SECURITY:**` bullet class, written like the others (a `summary:` line, an optional `off-switch:`; no qualifier). For an entry that has one, `release-check` requires
  `compat/compat.json`, in the same change, to either raise `min_safe` to this release (with the fix in `reasons`, and `min_safe_date` not before the entry's date) or carry a
  `min_safe_unchanged` record for this release with a reason. One decision covers every `**SECURITY:**` bullet of the entry. It is keyed by the version, so a release that is
  renumbered at merge has to move its record or the check fails. Why: `min_safe` is v0.102.2 (2026-09-27). 0.102.70 (2026-10-17) closed SSRF classification bypasses (NAT64, 6to4) and
  guarded `jsbundle`'s default client, and the floor did not move and no entry says why, so nothing tells a reader whether a pin that is below 0.102.70 is exposed or whether someone
  judged it fine. Which of the two is a human decision about go-common's own fixes; the check only makes sure that a decision exists.
- `compat/compat.json` gains the optional `min_safe_unchanged` list (`{"version", "reason"}`), and `compat` gains `Manifest.MinSafeUnchanged`, `FloorDecision` (covered by the floor,
  left alone on purpose with the reason, or undecided) and the validation of the records (a release version, a reason, once per release). The schema is still 1 and no record is
  added: older releases of the package decode the file with `encoding/json` and ignore the field (`TestMinSafeUnchangedIsInvisibleToAnOlderReader` pins that, and the check
  below ran the previous release's own tests against a manifest that has a record). 0.102.70 stays undecided; it is not for this change to decide it.
- `scripts/whatchanged` lists the `**SECURITY:**` bullets like the others, with the decision under each (`Floor: ...`). The one bullet of this kind that predates the class, in
  v0.80.0, was written `**SECURITY:` with the colon inside the bold, which the grammar already reads; it gets its `summary:` and so shows in `compat/changes.json`, as
  "covered by the floor".
- `scripts/release-check-selftest.sh` gains 13 cases (34 to 47 in all): no decision, a record for the release, one record for two bullets, a record for another release (the
  renumbering case), a record without a reason, the floor raised properly, without its reason listed, with the old date, and both raised and recorded, an unreadable `compat.json`, a
  `**SECURITY:**` bullet without a summary or with a qualifier, and an entry with no such bullet and an unreadable `compat.json` (not read).
- `docs/releasing.md` describes the class and the two ways to settle the floor.

Verified: every step of `.woodpecker.yml` against the pushed branch, by hand, in the pipeline's own images on the build host: `validate`, `race`, `lint` (0 issues), `vuln`, `forward-compat` on Go 1.27.2, `api`, `bench`, `release` and
`fuzz` (23 targets); the previous release's own `compat` package (v0.105.0, untouched) passes its tests, a 5 s `FuzzCheck` run and `compatcheck` against a `compat.json` that has a `min_safe_unchanged` record; ten deliberate breaks of the
rule and of the new `compat` code each fail a test or the selftest.

## [0.105.4] - 2026-10-20

A behaviour change now has to take a minor release: `scripts/release-check.sh` fails a patch release that carries one. Release tooling only; no package behaves differently.

### Added

- The rule: a change that claims a patch release (the same major and minor as the newest tag) and has a `**BREAKING (behavioural):**` bullet fails `release-check`, with the line of
  the bullet and the way out. Why: of the 10 behavioural entries since 0.100.0, 6 shipped in patch releases (0.102.69, 0.102.70 twice, 0.102.72, 0.102.73, 0.102.74); the other four
  were minors (0.103.0, 0.105.0). A service that followed "a patch is safe" took exit-code, metrics-label and SSRF-classification changes in five patches, and the version number
  gave no sign of it. It is rule 6 of `docs/api-policy.md`, which used to say "prefer a minor bump ... not something CI enforces yet". Only behaviour is gated: a `**BREAKING:**`
  (API) bullet, and one with another qualifier such as `(only for 0.102.59)`, may still be in a patch, as before.
- The exception, for a change a service can take in a patch (an urgent security fix is the case: waiting for a minor leaves services exposed): the bullet is marked
  `**BEHAVIOURAL (patch-safe):**` instead, and carries a `summary:` and a `reason:` line saying why a patch may carry it. It is the only form that marker has, and `reason:` belongs
  to it alone (`scripts/whatchanged` refuses both mistakes). These bullets are in `compat/changes.json` (class `behavioural`, with `reason`) and in the output of
  `scripts/whatchanged`, which prints the reason under the summary; the feed's schema is still 1, the field is additive.
- `scripts/whatchanged -release X.Y.Z -latest-tag vA.B.C` is where the rule lives, so that it is tested in Go with the rest of the grammar; `release-check.sh` passes it the newest tag.
  `scripts/release-check-selftest.sh` gains 12 cases (22 to 34 in all): a behaviour change in a patch, in a minor, in a major and in a patch after an untagged release (the rule is
  against the newest tag, not the entry below), two in one patch, patch-safe with and without a reason, with another qualifier, a reason on a BREAKING bullet, an API change and a
  bullet about one version in a patch (not gated), and no tag at all (nothing to compare with).
- `docs/api-policy.md` rule 6 and `docs/releasing.md` say it.

Verified: every step of `.woodpecker.yml` against the pushed branch, by hand, in the pipeline's own images on the build host: `validate`, `race`, `lint` (0 issues), `vuln`, `forward-compat` on Go 1.27.2, `api`, `bench`, `release` and
`fuzz`. The first `validate` run failed on `proxysupplier`'s `TestWebshareDirect_FailedInitialFetchIsRetriedWithBackoff`, a timing assertion in a package this change does not touch, while the host was busy; the whole step
passes on a rerun and that test passes 20 of 20 alone. Seven deliberate breaks of the rule (the patch test always true, any qualifier gated, minors gated too, `-latest-tag` not passed by the script, the reason not required, a
reason allowed on a BREAKING bullet, any qualifier accepted for patch-safe) each fail a test or the selftest.

## [0.105.3] - 2026-10-20

A per-package coverage ratchet in CI: a package that loses test coverage, or a new tested package nobody tracked, fails the pipeline of main (pull requests can run it by hand). Stacked on the CI stages change (0.105.1): it adds one step, to the second stage of main's pipeline. No package changes.

Why: statement coverage is 84.5% on average over 72 tested packages (v0.105.0) but nothing stopped a package from falling, and the weakest ones sit on the per-request path: `middleware` 67.6% (`server` wires it into 357 repositories), `response` 69.9% (163 importers), `openapi` 72.6% (99), and `spiffe` 34.9%, `shutdown` 56.7%, `backoffcoord` 58.9%.

### Added

- `coverage/floors.txt`: one line per tested package, `<package> <percent>`; initial values are the measured coverage minus 2 points, rounded down (`middleware` 65, `response` 67, `openapi` 70, `spiffe` 32). The 2 points are slack for timing-dependent tests: three runs of the whole suite on one tree differed by at most 0.5 points in a package (`server`).
- `scripts/coverage-ratchet.sh`: runs `go test -cover ./...`, prints a table (package, floor, now, status) and fails when a package is below its floor, when `go test` fails, or when a tested package has no floor yet. `--update` raises floors to measured minus the margin (`COVERAGE_MARGIN`, default 2) and never lowers one,
  adds new packages and keeps stale lines; it refuses to write while a package is below its floor or tests fail, so it cannot paper over a regression. A package with a floor whose tests are gone counts as 0%, below it. `--from FILE` judges a saved output without running Go; `COVERAGE_ADVISORY=1` prints the verdict but never fails (the rollback switch). It compares in tenths of a percent, so 82.0 against a floor of 82 is a pass.
- `scripts/coverage-ratchet-selftest.sh`, in the `validate` step: a fake `go` with canned output, 36 checks (rounding, never lowering, new and stale packages, a regression of 0.1 point, a failing `go test`, no-test and no-statement packages, `--from`, the margin, bad input). Six deliberate breaks of the script (`<=`, lowering on update, no margin, ignoring `go test`'s exit code, forgetting a package whose tests vanished, advisory always on) each fail it.
- A `coverage` step in `.woodpecker.yml`, **on pushes to main only**, after `validate` (second stage). It is another full test run, about 450 CPU-seconds, on a single agent shared with the fleet's pull requests (a pull request pipeline measured 400 s wall with queue contention), so pull requests do not pay it. The cost of that choice: a pull request can lower coverage and only main turns red, so `coverage/README.md` says to run `scripts/coverage-ratchet.sh` by hand before a change that removes or rewrites tests. A nightly run needs a cron schedule on the Woodpecker server and is not set up here. `COVERAGE_ADVISORY: "0"` is the step's switch.
- `coverage/README.md`: what to do when it fails, how to raise a floor, and how to turn it off.
- Tests, in a separate commit: `response` (`Success`, `ErrorResp`, `NewError`, `FromError`, `(*Error).Error`, the default observer) 69.9% to 90.4%, and `openapi` (`ScanDir`, hidden and vendor directories, an unreadable file, every `AddRoute` method) 72.6% to 93.4%; their floors are raised to 88 and 91 with `--update`. `middleware` (67.6%) is not touched here: another change owns that directory, so its floor stays at 65.

Verified: three runs of `go test -count=1 -cover ./...` in `golang:1.26` on the build host (72 packages, mean 84.6%, no failures); floors written from run 1 and runs 2 and 3 pass them; the selftest passes; `woodpecker-cli lint` (3.17) accepts the file. Line coverage is not quality: it keeps tests from disappearing, it does not say they assert anything.

## [0.105.2] - 2026-10-20

The `**BREAKING...**` bullets of this file now end with a one-line summary and, when there is one, the way back to the old behaviour. A generator reads them into `compat/changes.json`,
and `go run .../scripts/whatchanged <from> <to>` prints what changed between two versions as a section for a pull request body. Release tooling and one additive `compat` API: no
package behaves differently.

### Added

- Two field lines for a `**BREAKING...**` bullet, written as nested bullets at its end: `  - summary: ` (required, 1 to 200 characters: what changes for a service that bumps, and
  what to check) and `  - off-switch: ` (optional: the setting, option or call that restores the old behaviour without a release). They are appended, so no existing line of a bullet
  changed; the grammar, and what is refused, is in `docs/releasing.md`. Why: to put "behaviour changes since your pin" into the fleet's adoption pull requests the bullets were parsed
  and summarised by hand, and a first automatic truncation cut 0.103.0's bullet before its actionable half, `FLEET_EGRESS_DEFAULT_TRANSPORT=off`. The summary is the one line, and the
  off-switch is kept as a field of its own so that no shortening can drop it.
- `compat/changes.json`, generated from `CHANGELOG.md` by `go run ./scripts/whatchanged -gen` and embedded in the `compat` package: `compat.LoadChanges`, `ParseChanges` and
  `ChangeFeed.Between(from, to)` (releases newer than `from`, up to and including `to`; a pre-release or pseudo-version pin sorts below its own release, so that release's notes are
  included). Schema 1. A reader ignores fields and classes it does not know, so a later class cannot break a consumer that is older than it. It holds 12 changes, every bullet that
  starts with `**BREAKING`; `TestEveryBreakingBulletIsInTheFeed` counts them with a scan that shares nothing with the parser.
- `scripts/whatchanged` (Go, the standard library and `compat` only): `go run github.com/baditaflorin/go-common/scripts/whatchanged@latest <from> [<to>]` prints the section;
  `-gen` writes the file; `-check` exits 1 on a grammar problem or a stale file, as `scripts/pkgindex -check` does for `docs/PACKAGES.md`; `-release X.Y.Z` is what release-check runs.
  It refuses what it cannot place instead of skipping it: a marker it cannot read (`- **BREAKING**: x`, `**BREAKING CHANGE:**`, one on a wrapped line, in a paragraph or in a nested
  bullet), a field line written almost right (three spaces, a capital `S`, two spaces after the colon), a field given twice, a `## ` line that is not an entry heading, an unclosed
  code fence. A bullet that falls out of the feed unseen is the failure this exists to prevent.
- `scripts/release-check.sh` runs `scripts/whatchanged -release` for the version the change claims, so a `**BREAKING...**` bullet without its `summary:`, a field of more than 200
  characters (counted as characters, under any locale) or a `compat/changes.json` that is not what `CHANGELOG.md` says, in any entry, fails the `release` step.
  `TestFeedIsCurrent` in `scripts/whatchanged` is the same drift check inside `go test ./...`: it runs on a push to main as well, and needs no line in `.woodpecker.yml`.
- `scripts/release-check-selftest.sh`: runs release-check in throwaway git repositories with real tags, 22 cases (the version rules it already had, which had no test, and the new
  field rules). `TestReleaseCheckSelftest` runs it from `go test ./...` (not under `-race`), so every pipeline does.
- `docs/releasing.md` has the grammar, the commands and what CI does; rule 3 of `docs/api-policy.md` points to it.

### Changed

- The 12 existing `**BREAKING...**` bullets are backfilled with their fields (0.105.0 twice, 0.104.0, 0.103.0, 0.102.74, 0.102.73, 0.102.72, 0.102.70 twice, 0.102.69, 0.102.60 and
  0.63.0), from the one-line readings written for the adoption pull requests, cut to 200 characters. Five have an off-switch: 0.105.0's second bullet (a service that builds its own
  `EgressCollectors`), 0.104.0 (stay on 0.103.x), 0.103.0, 0.102.70's second bullet and 0.63.0. No other word of any entry changed.

Verified: every step of `.woodpecker.yml` against the pushed branch, one after the other, in the pipeline's own images on the build host: `validate`, `race`, `lint` (0 issues, and its selftest), `vuln`, `forward-compat` on
Go 1.27.2, `api` (no incompatible change: `compat` only gains identifiers), `bench`, `release` and `fuzz` (23 targets, 5 s each); `go run ./scripts/whatchanged v0.102.60 v0.105.0` lists the ten bullets of those
releases, 0.103.0's off-switch among them; 16 deliberate breaks of the parser, `Between`, the renderer and `release-check.sh` each fail a test or the selftest (the first run missed a code fence in column 0, which
has a test now). The steps were run by hand in those images, not by Woodpecker.

The rest of this entry, from here to the next version heading, is the 0.105.1 entry (the CI stages and `scripts/fuzz-changed.sh`, #191, tag `v0.105.1`). It was merged into this one and lost its heading; `release-check` does not accept a heading added below the top entry, so the text stays where it is.

The CI pipeline runs its steps in two stages instead of one after another, and a pull request fuzzes only the packages it touches. No package changes.

Why: a pull request pipeline took 358 to 576 s (six recent ones: 387, 412, 576, 358, 422 and 384 s, 733 s the day `forward-compat` was added). The steps ran one after another, and the `fuzz` step alone was
175 to 232 s of it, about half, to mutate every target for a change that mostly touches none: of the last 13 merges to main, 8 touched no package with a Fuzz target.

### Changed

- `.woodpecker.yml`: the steps now form a graph (`depends_on`). Stage 1 is `refs`, `validate`, `race`, `vuln` and `forward-compat`; stage 2 is `lint`, `api`, `bench`, `release` and `fuzz`, which wait for `refs`.
  Woodpecker 3.17 turns `depends_on` into stages (checked with `woodpecker-cli exec`: a step that depends on a fast step still starts only when every step of the previous stage is done), so a pipeline takes about as long as
  the slowest step of each stage. Concurrency is capped at five steps; the other four steps have no `depends_on`, which in a graph pipeline means "start now". The agent runs one workflow at a time
  (`WOODPECKER_MAX_WORKFLOWS=1`) but all steps of that workflow at once and is shared with the fleet's other pull requests; the CPU work is the same as before (a test run is about 450 CPU-seconds, three of them in stage 1),
  it now happens in a shorter burst. Every `when` is unchanged and the lint image is still pinned by digest. A step may only depend on a step that exists for the same event, or Woodpecker refuses the whole
  pipeline ("depends on step ... which is filtered out"): all of the steps that run for pull requests only depend on `refs`, which runs for both events.
- A step that fails now skips the next stage (it already skipped everything after it). Stage 1 is allowed to finish, so a pull request sees every stage 1 failure in one run.
- `fuzz` runs for pushes to main as well as pull requests. It had never run on main: a pull request ran every target, main ran none. Now a pull request runs `scripts/fuzz-changed.sh 5s` and a push to main runs
  `scripts/fuzz.sh 5s`, every target, so each is mutated at least once per merge. That adds the full fuzz run (about 190 s on its own) to a main pipeline; main pipelines gate nothing, and the step starts with stage 2, so it overlaps `lint` and `api`. A nightly run with a long
  `-fuzztime` needs a cron schedule on the Woodpecker server and is not set up here.

### Added

- `scripts/fuzz-changed.sh [fuzztime] [base-ref]`: fuzzes only the targets of the packages with a changed file (the diff from the base chosen by `scripts/lib/base-ref.sh`, the merge base with the target branch for a pull request,
  to the working tree; the old path of a rename counts; a file under `testdata/` belongs to its package, so a committed crash re-runs its target). It fuzzes every package when `go.mod`, `go.sum` or `scripts/fuzz.sh` changed or no base can be found,
  and passes at once when no touched package has a Fuzz target. A package that only imports a changed one is not fuzzed: 38 of 77 packages are, or are imported by, a package that has a Fuzz target, most of those imports incidental (`response`,
  `telemetry` and `secrets` are imported by four fuzzed packages that no target exercises through them), which would make the smoke nearly a full run again; `go test` still replays every seed corpus on every pull request.
  `FUZZ_ALL=1` and `FUZZ_DRY_RUN=1` are the escape hatch and the way to see what it would do. This is a real narrowing: a regression that only a mutation run of a dependent package's target would find is now found on the
  merge to main, not on the pull request.
- `scripts/fuzz-changed-selftest.sh`, in the `validate` step: a throwaway git repository and a fake `go`, 21 cases: which targets run for a change in a package with targets, without, nested, in `testdata/`, renamed, deleted or untracked,
  a nested module, a function named `Fuzzy`, the full-run triggers, no base, a dry run, and a pull request compared with its merge base while the target branch moved on. Five deliberate breaks of the script
  (no `testdata` exclusion, no nested-module exclusion, a looser target pattern, no `go.mod` trigger, diffing against HEAD instead of the working tree) each fail it.
- A `refs` step: in a pull request it fetches the target branch and the tags once, with three tries. The checkout holds the commit and its history but no other branch and no tag, so every step that compares with the
  target branch fetched it itself; with the steps side by side, five `git fetch` into one repository at once can fail on a ref lock. `lint` no longer fetches; `release-check.sh`'s own fetch finds nothing to do.

Verified: `woodpecker-cli lint` (3.17) accepts the file; `woodpecker-cli exec --local` on the build host ran the whole pull request pipeline (exit 0, all ten steps) and started the push pipeline's first stage; the selftest
passes in `golang:1.26`; `scripts/fuzz-changed.sh` was run over the last 13 merges to main (8 fuzz nothing, 4 fuzz 1 to 7 packages, 1 fuzzes everything). The measured pipeline durations are in the pull request.

## [0.105.0] - 2026-10-19

The egress and fleetfetch collectors no longer give every site a process fetches its own series: `host` is bounded at 32 heavy hitters per process, and the two histograms
lose `host`. This is the durable fix for the Metrics Hub Prometheus OOM loop of 2026-10-19 (19 kills at 2 and 3 GiB); services pick it up with the next go-common bump.

### Changed

- **BREAKING (behavioural):** `safehttp_egress_duration_seconds` is labelled `{service, via_proxy}`, without `host`, and `fleet_fetch_cache_age_seconds` is labelled `{service}`,
  without `host`. A query that filters or groups these two histograms by `host` now gets one series per service. go-fleet-metrics-hub's dashboards read both only as a fleet-wide
  `sum(rate(..._sum)) / sum(rate(..._count))`, which is unchanged. Why: a histogram costs a series per bucket, so per host it was the largest cost in the fleet's Prometheus:
  `safehttp_egress_duration_seconds_bucket` was 611,628 of 1.42 M head series on 2026-10-19 (go-fleet-metrics-hub #112 now drops it at scrape), and
  `fleet_fetch_cache_age_seconds_bucket` then became the largest metric, 145,170 series, after growing from 90k to 140k in three hours.
  - summary: `safehttp_egress_duration_seconds` and `fleet_fetch_cache_age_seconds` lose their `host` label: a query that filters or groups them by `host` now gets one series per service.
- **BREAKING (behavioural):** `host` on `safehttp_egress_requests_total`, `safehttp_egress_response_bytes_total` and `fleet_fetch_total` takes at most 32 named values per process
  (was 256), and a host gets one only on its 64th request, provided those 64 come before 1024 other unnamed hosts have been seen (the count restarts then). Every other request
  counts under `host="_other"`, so sums over `host` stay exact. Named hosts are never evicted: the label takes at most 34 values (`_other` and `_unknown` included) whatever a
  process fetches. Why: the old rule named the first 256 hosts a process saw. 92 of the 130 processes exporting `safehttp_egress_requests_total` sat at that cap (25k host values
  in all), 94% of their requests already counted under `_other` (97% for `fleet_fetch_total`), and every restart named a new random 256: the head held 74,295
  `safehttp_egress_requests_total` series for 34,406 live ones. The hosts that recur (upstream APIs, the selftest target, deep-crawled sites) keep their names, so
  `topk(15, sum by (host) (...))` on the fleet-egress dashboard shows them instead of the first domains crawled after boot. The cost: a process's first 63 requests to a host
  count under `_other`, so a host a low-volume service calls rarely gets its name late or never.
  - summary: `host` on `safehttp_egress_requests_total`, `safehttp_egress_response_bytes_total` and `fleet_fetch_total` takes at most 32 named values per process (was 256); every other host counts under `_other`.
  - off-switch: for an `EgressCollectors` a service builds itself, `promx.WithHostLimit(256)` and `promx.WithHostMinHits(1)` restore the old naming; `fleet_fetch_total` takes no setting
- `WithHostLimit(n)` with `n <= 0` keeps the default (now 32); it meant 256 before.
- A host without a slot, once every slot is taken, costs one read-locked map lookup. It took the write lock on every call before, which for a crawler was 94% of its requests.

### Added

- `promx.WithHostMinHits(n)`: how many requests a host needs before it gets its own `host` value (default 64). `WithHostMinHits(1)` restores first-come naming for an
  `EgressCollectors` a service builds itself. The fleetfetch collectors always use the defaults.
- Tests: `TestEgressSeriesBound` and `TestFleetFetchSeriesBound` run one synthetic crawler process (144,224 requests over 20,003 hosts, shaped on the live per-host distribution:
  most domains fetched 1-10 times, every 100th 100 times, three upstreams taking every 10th request) through the default collectors and count the series Prometheus would store.
  The same test file run against 0.104.1: 7,960 egress series (6,804 of them the duration histogram, 258 host values) and 3,256 fleetfetch series (2,761 the cache-age histogram),
  against 219 and 101 here; the upstreams keep their names and no domain fetched 1-10 times gets one. `TestHostCardCapBound` feeds both admission rules four adversarial inputs
  (every host new; each host exactly `minHits` times in a row; round robin over 1000 hosts; empty hosts mixed in) and pins at most `limit` named values plus `_other` and
  `_unknown`, and at most `window` candidates tracked; a concurrent variant runs under `-race`.

Expected effect on the Metrics Hub (measured 2026-10-19): these five metric families hold 222,880 live series (about 470k of the 967k head series, counting the ones restarts leave
behind); with every service on 0.105.0, an estimated 3k to 14k live. The rollout is a separate `fleet-runner update-dep` / `rollout`. Once it is done, go-fleet-metrics-hub's
scrape-time drop of `safehttp_egress_duration_seconds_bucket` can go: without `host` the buckets are about 3,100 series fleet-wide.

## [0.104.1] - 2026-10-19

`middleware.KeystoreOpts` and `server.WithKeystoreAuthPublicPrefixes` let a service declare request path prefixes that bypass the keystore gate, for a handler whose own unguessable value is its access control.

### Added

- `KeystoreOpts.PublicPathPrefixes []string`: a prefix match checked in `TokenAuthKeystore`'s existing bypass step, alongside the fixed `/health`, `/version`, `/selftest`, ... list. `""` and `"/"` are
  ignored rather than honored — either would make the whole service public by a configuration mistake, which this field exists to avoid, not enable.
- `server.WithKeystoreAuthPublicPrefixes(publicPrefixes []string, localTokens ...string) Option`: `WithKeystoreAuth` plus the field above, with the same `PromAuthCollectors` auto-wiring.
  Why: `go-pentest-oob-collector`'s public out-of-band catch endpoint, `GET /c/<token>[/<probe-id>]`, is answered 401 by the keystore gate before its handler ever runs — the caller is a vulnerable
  scanned target reporting a callback, which can never be handed this service's own key. The token in the path is the access control (random, one-shot, TTL-bounded, rate-limited; unknown and expired
  tokens both answer the same 1x1 GIF so a prober cannot distinguish live from dead), not a keystore credential. The gateway's own `auth_request` has carried the same gap on its side since the
  endpoint was built; closing it is a `go_fleet_runner` nginx-template change, not a `go-common` one. Declaring the prefix here only reaches the container-level gate; the public fleet vhost still
  needs the matching template declaration before an external caller can reach it. `"/c/"` exempts `/c/<token>/<id>` too (prefix, not exact); never declare a prefix that also serves a keystore-gated
  route under it.
- Tests: the prefix bypasses with no key; an unrelated path (including one that merely contains the prefix string later in its path) still requires one; `""`/`"/"`/both never widen the whole
  service; the option wired end to end against `srv.Handler()`. Two deliberate reverts (drop the bypass check, drop the `""`/`"/"` guard) each fail a test.

Verified: `apidiff-check` reports no incompatible API changes (a new struct field and a new exported function are additive); the four deliberate breaks above each fail the test that names them.

## [0.104.0] - 2026-10-18

The minimum Go version is now 1.26 (it was 1.25).

### Changed

- **BREAKING:** `go.mod` says `go 1.26.0`. A service that adopts this release needs a Go 1.26 builder (`golang:1.26` or newer), and `go get` raises its own `go` line to 1.26.0. The official `golang`
  images set `GOTOOLCHAIN=local`, so an older image fails with `go: go.mod requires go >= 1.26.0 (running go 1.25.14; GOTOOLCHAIN=local)` instead of downloading a toolchain. A service that cannot move
  yet stays on 0.103.x, and `scripts/backport.sh` cuts a `release/0.103` line only when a real service needs a security fix (`docs/releasing.md`).
  Moving the builder image to 1.26 first, without touching the `go` line, is safe in that order: an image newer than the `go` line always builds.
  - summary: `go.mod` says `go 1.26.0`: a service that adopts this release needs a Go 1.26 builder (`golang:1.26` or newer), and `go get` raises its own `go` line to 1.26.0.
  - off-switch: stay on 0.103.x; `scripts/backport.sh` cuts a `release/0.103` line only when a real service needs a security fix (docs/releasing.md)
- A service's own `go` line decides the default of each `GODEBUG` setting, so raising it to 1.26.0 flips four defaults for that service (each can be put back with a `godebug name=value` line in its go.mod):
  `urlstrictcolons` (`net/url.Parse` now rejects a host with colons outside brackets, such as `http://localhost:1:2`), `cryptocustomrand` (most `crypto/*` APIs ignore a custom random `io.Reader`),
  `tlssecpmlkem` (the default TLS key exchange offers two more hybrid post-quantum groups, SecP256r1MLKEM768 and SecP384r1MLKEM1024: the ClientHello is 4 bytes larger and its JA3/JA4 fingerprint changes, and no extra key share is sent; measured with a Go 1.26 client against a local listener), and
  `httpservecontentmaxranges` (`http.ServeContent` allows up to 200 ranges per request).
- CI builds on `golang:1.26`. The `forward-compat` step now tests the next line, Go 1.27 (`FORWARD_COMPAT_GO_MINOR`), so the step keeps showing what Go changes before the minimum moves again.

### Fixed

- Four reachable `golang.org/x/net` HTTP/2 advisories (GO-2026-6603, GO-2026-6611, GO-2026-6612, GO-2026-6617) are closed: `x/net` goes from v0.58.0 to v0.61.0, which needs `go 1.26.0`, and the
  allowlist in `scripts/govulncheck-allow.txt` is empty. Taking `x/net` also moves `x/sync` to v0.24.0, `x/crypto` to v0.58.0, `x/text` to v0.43.0 and `x/sys` to v0.49.0.

## [0.103.3] - 2026-10-18

`safehttp`'s trace and backoff posts carry the service's own key, so a scanner's call trace and backoff consult reach the tracer and the coordinator instead of being refused with 401.

### Fixed

- `WithTraceCollector` (POST `<url>/traces`) and `WithBackoffCoordinator` (POST `<url>/backoff`) sent no `X-API-Key`. `go-fleet-call-tracer` and `go-fleet-backoff-coordinator` sit behind the
  keystore, so every post was answered 401 (on a replica that reaches them by their public name the gateway refuses it first, on the same host the container does), the span or the consult
  was dropped, and the only trace of it was one `safehttp: trace emit failed: collector returned 401` line a minute. The posts now carry `FLEET_API_KEY`, the key a service already sends to
  every sibling it calls. The key goes only to a fleet host (`egress.Direct`: a name under the fleet's domains, a bare container name, a loopback, private or tailnet address), never to a
  URL a caller configured that points elsewhere; a key already on the request is kept; an unset variable or the `default_token` placeholder sends nothing, as before. The post is still
  bounded by the same connect and read budget and still fails open: a refused post changes nothing for the caller. A service whose key does not carry the tracer's or the
  coordinator's vhost in its scope (the gateway checks `X-Expected-Scope` against it) is refused exactly as before, so a key has to be granted that scope for the trace to arrive.
- Not changed: `backoffcoord.Client` (the standalone client for non-HTTP code paths) has the same keyless post; no repository in the organisation imports it, and its default URL differs from
  the name the services are configured with, so it is left for the day a service does.

### Added

- Tests: the trace post and the backoff consult carry the key (a stub tracer and a stub coordinator record the header); no key is sent when `FLEET_API_KEY` is unset, blank or
  `default_token`; the helper's rule for fleet and third-party hosts (the fleet apex, a public fleet name, a bare container name, a private and a loopback address get the key; a
  foreign host, `0exec.com.evil.example`, `evil0exec.com` and a public address do not), a key the caller already set is kept, the value is trimmed, and a nil request does not
  panic. Six deliberate breaks of the change (dropping either call, the fleet-host guard, the placeholder guard, the existing-key guard or the trim) each fail a test.

Verified: the new tests with `-race`; the six breaks above; against the live services a keyless post to the tracer and to the coordinator is 401 on the dockerhost and the same post with
a service key passes authentication (400 for an empty body).

## [0.103.2] - 2026-10-18

A CI check for the behaviour changes Go makes for a newer `go` line, so that a floor move does not find them first. No package changes.

### Added

- `scripts/forward-compat.sh [GO_MINOR]`: copies the working tree to a temporary directory, runs `go mod edit -go=<GO_MINOR>.0` there, then `go vet ./...` and
  `go test -count=1 ./...`, and ends with the packages and tests that failed. The checkout is never edited and the raised line is never committed. Why: Go takes the default of
  each `GODEBUG` setting from the `go` line of the module it builds, not from the toolchain, and go.mod says `go 1.25.0`, so the suite on a 1.26 or 1.27 toolchain still ran
  with the 1.25 defaults. With the line at 1.26 `proxysupplier`'s `TestSelfProxyGuard_Loopback` fails (`urlstrictcolons`: `url.Parse` rejects `http://::1:3001/`, so the
  self-proxy guard failed open; fixed in 0.103.1), and nothing in CI showed it. GOTOOLCHAIN is forced to `local`, so the Go that runs is the Go that is tested; it exits 2, before running anything,
  for a toolchain older than the line asked for, a line that is not newer than go.mod's (a check that tests nothing is worse than none), and an argument that is not a minor
  version. `FORWARD_COMPAT_KEEP=1` keeps the copy to debug in. Only the root module is covered (`lint/fixture` is a module of its own).
  `FORWARD_COMPAT_GO_MINOR=1.26` in `scripts/tool-versions.sh` is the default minor.
- `scripts/forward-compat-selftest.sh`, in the `validate` step: runs the script with a fake `go` and checks that the checkout is byte-identical afterwards and the copy is gone,
  that vet and test run in the copy (without `.git`) on the requested line and with `GOTOOLCHAIN=local` even when the caller sets `auto`, that failing tests or a failing vet exit
  1 and are named while the other still runs, and that the three refusals exit 2 without running Go. Five deliberate breaks of the script (editing the checkout, not forcing the
  toolchain, swallowing the test exit code, a wrong line, copying `.git`) each fail it.
- A `forward-compat` step in `.woodpecker.yml` (pull requests and pushes to main, `golang:1.26.9`). **It blocks.** It was first written as informational (`failure: ignore`) because
  main still failed it with the one `proxysupplier` test above, and a blocking step would have turned every pull request red until the 0.103.1 fix merged. That fix is on main, the
  script passes on Go 1.26 and 1.27, and the step no longer carries `failure: ignore`.
- `docs/releasing.md` has a section on the check: what it is for, how to run it (`scripts/forward-compat.sh 1.27`) and what to do when it fails (fix the code; a `godebug` pin in
  a service is the last resort).

Verified: the script on this tree, rebased onto the 0.103.1 fix, with Go 1.26.9 and Go 1.27.2 in the `golang` images: `go vet` is clean and every test passes on both; before the fix
both failed on `proxysupplier` `TestSelfProxyGuard_Loopback` alone; the checkout and the synced tree both still say `go 1.25.0` afterwards; `woodpecker-cli lint` (v3.17.0) accepts the file.

## [0.103.1] - 2026-10-18

`proxysupplier` writes an IPv6 proxy host in the form Go 1.26 accepts, so its self-proxy guard fires for `::1` again on a service built with a go 1.26 line.

### Fixed

- `proxysupplier` built the proxy URL from `PROXY_HOST` / `PROXY_PORT` with `"%s:%s"`, so an IPv6 host gave `http://::1:3001/`, which is not a URL. Go 1.26 changed the default of
  GODEBUG `urlstrictcolons`: `net/url.Parse` refuses a host with a colon outside brackets (for a module whose go line is 1.26 or later; go-common's own is still 1.25). The guard
  against a proxy that routes back to this process (`isSelfProxy`) reads a parse error as "not this process", so for the loopback host `::1` it stopped firing on such a service and
  `NewFromConfig` kept a `plain_proxies` / `env` supplier pointing at itself. `buildURL` now brackets an IPv6 host (`http://[::1]:3001/`); a name and an IPv4 host give the same URL
  as before, and the user name and password are written as before. Found by running go-common's own tests with a `go 1.26.0` line, where `TestSelfProxyGuard_Loopback` failed.
- `webshare_direct`: an IPv6 `proxy_address` is bracketed the same way, so the URL parses and the pool key is the `[host]:port` form that `conn.RemoteAddr()` reports, which is what
  `MarkResult` expects. Webshare lists IPv4 addresses today; nothing changes for those.
- Tests: `buildURL` for a name, an IPv4 host and an IPv6 host with and without credentials; the guard for `::1` with the strict parser forced (`t.Setenv("GODEBUG",
  "urlstrictcolons=1")`, which `net/url` reads again at run time; it needs a Go 1.26 toolchain and skips on older ones); and the webshare IPv6 entry.

## [0.103.0] - 2026-10-18

A plain `http.Client` now follows the same proxy rule as the fleet's own clients: requests to the fleet's own hosts go direct, everything else still follows the environment.
This is the first release tagged since v0.102.68; the entries 0.102.69 to 0.102.74 below were merged without tags and ship with it.

### Changed

- **BREAKING (behavioural):** `server.New` calls `egress.InstallDefaultTransport()` first, before anything else in the process starts a request. It replaces the proxy function of
  `http.DefaultTransport` (the standard one, `http.ProxyFromEnvironment`, which applies `NO_PROXY` and nothing else) with `egress.Proxy`. Everything that uses the default transport
  follows it: `http.DefaultClient`, an `http.Client` with no `Transport`, `http.Get`, the dependency probes services register with `depcheck`, and libraries that build their client the
  standard way. A destination that is ours (a bare container name, a loopback, private or tailnet address, `host.docker.internal`, a name under 0exec.com, 0crawl.com, 0own.com,
  0docker.com or 0infer.com, or a domain in `FLEET_DIRECT_DOMAINS`) is now direct; anything else still goes where `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` say.
  Why: a service that runs with an egress proxy and calls a sibling by a name that `NO_PROXY` does not list sent that call through the proxy. A bare container name reached the proxy,
  which cannot resolve it (502); a public fleet name reached the gateway from the proxy's exit address, which a vhost with a source allowlist refuses, or which fails on and off as the
  proxy rotates its exits (a dependency probe that failed 4 runs in 16). An audit of the URLs in the environment of the containers on two runtime hosts found 23 and 16 destination hosts
  that a plain client would have sent to the proxy. `egress.Proxy` already gave the fleet-direct rule to the Graph, the fetch cache and the rate coordinator; this gives it to the rest,
  so one release closes the gap for every service instead of a `NO_PROXY` edit per service and host. A service with no proxy variables set is unchanged, since there is no proxy to skip.
  What is not affected: `safehttp` builds its own transport with its own proxy policy (it is the public-web client, and the runner's `proxy_egress` rule keeps our public names on the
  proxy for scanners that use it), `secrets.DirectTransport`, `egress.Transport()` and `fleetfetch`'s transports, third-party destinations, and a program that set its own proxy function
  on the default transport (`nil`, a fixed proxy, a wrapper): that choice is kept, because replacing it could only widen the use of the proxy. A service that does not call `server.New`
  (a few proxies and brokers) is unchanged until it calls `egress.InstallDefaultTransport()` itself.
  To go back for one service without a release, set `FLEET_EGRESS_DEFAULT_TRANSPORT=off` (also `false`, `0`, `no`, `disabled`; any value that is not an "on" spelling reads as off, and the
  start-up check names a value it does not recognise). `server.New` logs one line saying what it did, never a proxy value.
  The write happens once, early, because `net/http` reads the proxy function without a lock: a service that starts requests on the default transport before `server.New` runs can race
  with it (the race detector reports it; in practice it is one word written once), and should call `egress.InstallDefaultTransport()` at the top of `main` instead.
  - summary: `server.New` sends fleet-internal destinations (container names, private and tailnet addresses, our domains) direct on `http.DefaultTransport`; the rest still follows `HTTP_PROXY`/`NO_PROXY`.
  - off-switch: `FLEET_EGRESS_DEFAULT_TRANSPORT=off` (also `false`, `0`, `no`, `disabled`) in the service's environment, no release needed

### Added

- `egress.InstallDefaultTransport() InstallStatus`: the call above. Idempotent and safe for concurrent callers; the status (`DefaultInstalled`, `DefaultAlready`, `DefaultDisabled`,
  `DefaultCustom`, `DefaultForeign`, with `Active()` and `String()`) says what happened and why. `egress.DefaultTransportInstalled()` reports whether the default transport follows the rule,
  and `egress.DefaultTransportEnv` names the switch.
- `FLEET_EGRESS_DEFAULT_TRANSPORT` and `FLEET_DIRECT_DOMAINS` are declared in the `env` registry, so they are in `docs/ENV.md` and the start-up check flags a likely typo of either. The
  parsing of `FLEET_DIRECT_DOMAINS` is unchanged.
- Tests: the install replaces only the standard function (`nil`, a fixed proxy and a wrapper are left alone, as is a transport that is not an `*http.Transport`); the switch and its
  spellings; the status of 64 concurrent callers (exactly one installs); a real client against an origin and a recording proxy, showing that a fleet name, a bare container name, a
  private address and a tailnet address go straight to the origin while `example.com`, a look-alike domain and an API host go to the proxy, with the stock transport as the contrast;
  and in `server`, that `New` calls the install once per process however many times it is called, that the default transport follows the rule afterwards, and that the log line says what
  happened without carrying a proxy value. Nine deliberate breaks of the code (no assignment, replacing a custom function, ignoring the switch, remembering a failed install, dropping the
  call from `New`, no once, always-equal and nil-only comparisons, an unrecognised value reading as on, a log line with the proxy) each fail a test.

## [0.102.74] - 2026-10-18

Metrics-wiring fixes for the problems the wave-4 observability review found ([docs/observability.md](docs/observability.md), "Known limits"). One behaviour change: the
`method` label of the `promx` HTTP collectors.

### Changed

- **BREAKING (behavioural):** the `promx` HTTP collectors (`http_requests_total`, `http_request_duration_seconds`, `http_response_size_bytes`, so every service built on `server.New`)
  label a request `method="OTHER"` when its method is not one of GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS, CONNECT or TRACE. They used to label it with whatever token the client
  sent. Go's server accepts any token as a method and the middleware runs before authentication, so anyone who could reach the port could create a new series set per distinct
  method: a probe with 300 methods made 300 series sets, and about 900 requests made a 2 MB scrape. Methods are case-sensitive, so `get` is `OTHER` too. The request is still
  counted and only the label changed: a dashboard or alert that selects a non-standard method by name (`method="PROPFIND"` on a WebDAV service) must select `"OTHER"` instead.
  The nine standard methods are unchanged.
  - summary: the `promx` HTTP metrics label a request `method="OTHER"` when its method is not one of the nine standard ones: a dashboard or alert that selects a non-standard method by name must select `"OTHER"`.

### Added

- `server.WithPromRoute(fn, limit)`: sets the route function and the limit of distinct route values for the Prometheus HTTP collectors of one server. `server.New` had no way to
  do either (`promx.AutoWire` takes no options, and `WithRouteFunc` and `WithRouteLimit` only work on a collector you build yourself), so every `server.New` service labelled
  routes with the raw path, kept the first 512 and folded the rest into `_other`. The default is unchanged. Its comment says that `GET /metrics` is unauthenticated, so a token
  embedded in a path is a readable label value, and that `graph` templates paths while `promx` does not.
- `promx.(*HTTPCollectors).WithRoute(fn, limit)`: a view of existing collectors with its own route function and limit, recording into the same series. The collectors it was
  called on, and the set `AutoWire` shares, do not change.

### Fixed

- `obs.Init()` before `server.New` left the debug server's `/metrics` mirror empty for the life of the process (0 bytes against 16 KB when `Init` came after), because it looked
  the `promx` registry up once, when it started, and `promx.Init` had not run yet. It looks the registry up on every scrape now, so the order does not matter. pprof was not affected.
- The package comment of `promx` said `telemetry.Init` calls `AutoWire`. It does not; `server.New` does, and so does a direct call. The comment says so.
- The comment on `Server.PromHTTPCollectors` said a caller could supply a custom `RouteFunc` through it. Replacing the field after `New` never did anything; the comment now
  points to `WithPromRoute`.

## [0.102.73] - 2026-10-18

`server.Run` and `server.RunTier` no longer let a service that cannot serve end with exit status 0 and no log line.

### Changed

- **BREAKING (behavioural):** `server.Run` and `server.RunTier` now log the error that `Server.Start` returns and exit with status 1. Before, they dropped it: `main` returned and the
  process ended with status 0 and no message, so a supervisor read a service whose port was taken as having stopped cleanly. This covers a listen failure and a drain that runs out of
  time after SIGTERM (`Start` already logged that one and returned the error; the process now ends with status 1 instead of 0). A restart policy that restarts only on failure
  (Docker `restart: on-failure`) will now restart such a service, so a port that stays taken becomes a restart loop with the reason in the log, where it used to be a silent exit.
  A service that calls `srv.Start()` itself is unchanged: with `WithGracefulDrain` a listen failure already ended the process with status 1 (`log.Fatalf`), and without it `Start`
  returns the error to its caller, as its documentation says.
  - summary: `server.Run`/`RunTier` now log the error from `Server.Start` and exit with status 1 (before: status 0, no message), so a restart-on-failure policy restarts a service whose port is taken.

### Fixed

- The `Server.Start` documentation said `Run` and `RunTier` ignore the error `Start` returns; it now says what they do.

## [0.102.72] - 2026-10-17

Wave 4 of the go-common hardening: a CI budget on what the small packages may import, a guide to the observability packages, a count of which packages are used, and two
deprecations. One behaviour change: three call sites now return an error, not a panic, when a required proxy is missing.

### Changed

- **BREAKING (behavioural):** `client.Fetch` (the direct path) and `jsbundle.Recover` / `RecoverAll` no longer panic when proxy egress is required (`safehttp.RequireProxy`
  or `FLEET_REQUIRE_PROXY=1`) and no proxy variable is set. They run per request and in worker goroutines, where a deployment mistake should not take the process down. `Fetch`
  and `Recover` now return an error and fetch nothing (still fail-closed); `RecoverAll` logs the reason and returns nil. A caller that relied on the panic to stop the process
  must check the error. The start-up constructors, `safehttp.NewClient` among them, still panic as before.
  - summary: `client.Fetch` and `jsbundle.Recover`/`RecoverAll` return an error instead of panicking when proxy egress is required and no proxy variable is set; a caller that relied on the panic must check it.

### Fixed

- `jsbundle`: after the default client failed to build once, every later `Recover` call died with a nil-pointer dereference. A failed build is no longer cached.
- `Server.Start`: its documentation said `log.Fatal` was no longer used. It still is, on a listen failure with `WithGracefulDrain`; the comment now says what the code does,
  and that `Run` and `RunTier` ignore the error `Start` returns. The `WithAgentFromEmbed` panic that cannot happen now says so.

### Added

- `safehttp.NewClientE`: `NewClient` that returns an error where `NewClient` panics (`WithoutProxy` with `RequireProxy`, or a required proxy with no proxy variable set). The error
  text is the text `NewClient` panics with, and no client is returned with an error. For clients built lazily on a request path, and for checks that should report a problem, not crash.
- `scripts/dep-budget.sh` and `scripts/dep-budget.txt`: a budget on the third-party modules each small package may reach. A package that gains one fails `validate`, and the message
  names the package, the module and the go-common package that imports it. `scripts/dep-budget-selftest.sh` proves the gate fails when it should. `docs/dependencies.md` is
  generated from the same data (`scripts/dep-budget.sh --write-doc`; CI fails if it is stale), and `docs/adr/0002-dependency-budget.md` records the measurements and why nested
  modules are not worth it yet.
- `docs/observability.md`: which of `promx`, `metrics`, `reqstats`, the access log, `logx`, `telemetry`, `graph`, `obs` and `profiling` emits what, what `server.New` wires by default and
  how to turn each off, the per-request cost of each, and why `metrics` is not deprecated. Documentation only.
- `docs/package-usage.md` and `scripts/package-usage.sh`: how many repositories import each package (a lower bound; counts only), and how to refresh it.

### Deprecated

- `shutdown`: use `server.New` and `Server.Start` (`WithDrainTimeout`, `WithGracefulDrain`), or `signal.NotifyContext` and `http.Server.Shutdown`. Removal no earlier than v0.104.0.
- `client.Client`, `client.New` and `(*Client).Post`: use `safehttp.NewClient` or `meshclient`. `Post` never sent its body. The rest of `client` (`Fetch`, the `JSProxy*` helpers and
  the capability types) has no replacement and is not deprecated. Removal no earlier than v0.104.0.

## [0.102.71] - 2026-10-17

Wave 3 of the go-common hardening: the building blocks for calling a sibling service well, a catalogue of environment variables, and structured logs.
Everything is additive and the defaults are unchanged: nothing is switched on for a service that does not ask.

### Added

- `meshclient`: the HTTP client for calls to sibling services on the private mesh, where `safehttp` cannot be used because it refuses private addresses. The caller must
  choose `FailOpen` or `FailClosed` (there is no default). A failure that means the sibling is unusable (connection error, timeout, 5xx or 429, an unreadable reply, or a
  refusal by the resilience policy) is an `*UnavailableError` (`meshclient.IsUnavailable`); in fail-open mode it also appends `<name>-down`, `-timeout`, `-rate_limited` or
  `-degraded` to the request's `degraded.Sink`. A 4xx is a `*StatusError`, never "unavailable". Response bodies are capped, JSON is decoded strictly (one value, nothing after
  it), redirects are not followed, and the default transport carries trace context and fleet-graph events. `degraded` gains `NewContext` and `FromContext` for this.
- `resilience`: one policy over the existing pieces. A load-shed gate, a circuit breaker and retries limited by a **retry budget** (a token bucket that gets a fraction of a token
  per call and a small steady refill), so that during an outage retries stay a small share of traffic instead of multiplying it by the attempt count. `resilience.Retryable` is the
  single answer to "can a retry fix this?" (an error's own `Retryable() bool`, `retry.Transient`, a fleet error's HTTP status, network failures); only upstream-trouble failures
  count against the breaker, a 404 does not. `docs/outcomes.md` says which of `errors`, `meshresult`, `degraded` and `nullify` to use for what.
- `env` registry: packages declare their variables (`env.Declare`, `env.Removed`), and the process can list them, validate the ones that are set and show them redacted.
  Validation catches a value that does not parse, a removed variable that is still set, and a likely typo of a declared name (`GRAPH_COLLECTER_URL`), all of which were silent
  before. Redaction hides a variable declared secret, any whose name looks like a credential, and any whose value carries one. `server.New` runs the check once at start-up and
  only logs; the `obs` debug server (localhost only) serves it at `/debug/config`; `go run ./scripts/envdoc` generates `docs/ENV.md`, and CI fails if it is stale or a package that
  declares variables is not imported by the generator. `graph` (13 variables) and `runtimetune` (6) are declared as the first users, and `GRAPH_USE_EGRESS_PROXY`, removed in 0.102.60,
  is recorded as removed. Their parsing is unchanged; their tests pass untouched, and new tests keep each declared `Check` in step with the real reader.
- `logx`: structured logging. `logx.Install` makes slog JSON the process logger with the service, version and node on every line and the trace id inside a traced request, and,
  because `slog.SetDefault` also redirects the standard library, makes every existing `log.Printf` structured too. Credentials are redacted from string attributes and from the
  message (bearer tokens, URL user:password, `token=` and `password=` values). It is opt-in: `server.New` installs it when `LOG_FORMAT=json` (`LOG_LEVEL` sets the level) or the
  service passes `server.WithStructuredLogs()`; otherwise output is unchanged. `middleware.SetLogger` replaces the access-log logger, and the access log now logs with the request's
  context so the trace id reaches it; its output is byte-for-byte the same without logx.
- Fuzz targets for the new parsers (`logx` redaction idempotence, the `env` secret-never-leaks property, `compat`, the `meshclient` response decoder), benchmarks for
  `resilience` and `logx`, and runnable examples for `meshclient`.

## [0.102.70] - 2026-10-17

Wave 2 of the go-common hardening: a shared SSRF test corpus (which found a hole in the guard), tests for the packages that had none, hot-path
benchmarks with an allocation gate, a security floor for pins, and release tooling. Two behaviour changes, both marked below.

### Fixed

- **BREAKING (behavioural):** `safehttp.IsBlocked`, and so `GuardHost`, `CheckURL`, the guarded dialer and `egressproxy.DefaultGuard`, now also
  refuses an IPv6 address that carries a blocked IPv4 address through a translator: NAT64 (`64:ff9b::/96`) and 6to4 (`2002::/16`). Before, `64:ff9b::7f00:1`,
  `64:ff9b::a9fe:a9fe` (the cloud metadata address) and `2002:7f00:1::` were allowed. Reaching them needs a NAT64 or 6to4 gateway on the path, which a typical
  container host lacks, so this closes a latent hole rather than a live one. A NAT64 or 6to4 address that carries a public IPv4 address is still allowed. Found by
  the new SSRF corpus; `FuzzIsBlocked` now checks the embedded forms too.
  - summary: `safehttp.IsBlocked` (so `GuardHost`, `CheckURL`, the guarded dialer, `egressproxy.DefaultGuard`) also refuses IPv6 addresses that carry a blocked IPv4 address through NAT64 or 6to4.
- **BREAKING (behavioural):** `jsbundle.RecoverOptions.Client`, when nil, is now a guarded `safehttp` client (20 s timeout, refuses internal addresses, also
  after redirects) instead of `http.DefaultClient`, which had no timeout and no SSRF guard, while the URLs it fetches come from pages and error reports. Pass a
  `Client` to opt out, for example an `httptest` server's client in a test.
  - summary: `jsbundle.RecoverOptions.Client`, when nil, is a guarded `safehttp` client (20 s timeout, refuses internal addresses) instead of `http.DefaultClient`.
  - off-switch: set `RecoverOptions.Client` (for example an `httptest` server's client in a test)
- `config.Load` read `service.yaml` naively: any indented `name:` or `version:` replaced the service's own (the template's `owner.name` did), a trailing
  `# comment` stayed in the value, and a missing file printed `DEBUG: Failed to open service.yaml` to stdout. Only top-level keys count now, comments and quotes are
  stripped, and a missing file is silent. It only changed the result for callers that pass an empty app name.

### Added

- `testhelpers/ssrfcorpus`: one table of SSRF probe targets (loopback, IPv4-mapped, NAT64 and 6to4 IPv6, RFC 1918, link-local and cloud metadata, CGNAT, ULA,
  alternative spellings of 127.0.0.1, and the public addresses just outside each range), run against `safehttp` (host guard, `IsBlocked`, `CheckURL`, the client at dial
  time), `egressproxy.DefaultGuard` and `jsbundle`'s default client. Add a case there when a bypass is found anywhere.
- `testhelpers/leakcheck`: a dependency-free goroutine-leak check (`leakcheck.Check(t)`), used by new leak tests for `egressproxy` tunnels, `workpool` and `shutdown`.
- Tests for `shutdown` (signal drain, drain timeout, startup error), `config`, `header` (every constant is a well-formed, unique header name), `version` and the
  `httpmock`, `fakekey`, `fakegraph` and `testclock` helpers. `t.Parallel` on 68 tests in seven pure packages.
- Benchmarks for the per-request path: `BenchmarkServerRequest` puts the whole `server.New` stack (telemetry, request id, metrics, body limit, promx, graph) at about
  79 allocations and 5.9 KB per request around a no-op handler, access log excluded; plus `middleware`, `reqstats`, `promx`, `response`, `validate`, `cache`, `pagination`
  and `safehttp`. `scripts/bench-gate.sh` and the `bench` CI step (pull requests) fail when a benchmark's allocs/op grows by more than 10% and at least 2 against the
  merge base; ns/op is printed but not gated, because time on a shared runner is not reproducible.
- `compat` package, `compat/compat.json` and `scripts/compatcheck`: the oldest go-common version with every security fix go-common itself shipped (`v0.102.2`, the
  gRPC patch), with a check for a service's `go.mod` that understands tags, pre-releases and `v0.0.0-<time>` pseudo-versions, and optional per-line floors for old
  lines that get backports. It says nothing about dependency or toolchain advisories, which stay in `scripts/govulncheck-allow.txt`.
- `scripts/release-check.sh` and the `release` CI step (pull requests): the version a change adds to `CHANGELOG.md` must be well formed, not already tagged, and newer
  than the latest tag and the entry below it. `scripts/backport.sh` prepares a security backport branch in a separate worktree and pushes nothing. `docs/releasing.md`
  describes both. `scripts/lib/base-ref.sh` is the one place the gates choose their base (merge base in a pull request), shared by the API, benchmark and release checks.

## [0.102.69] - 2026-10-17

Wave 1 of the go-common hardening: CI gates, fuzz targets, an API policy, a shared lint config and generated docs. Three bugs the new fuzz
targets found are fixed (one changes behaviour: see `validate` below). No exported API changed.

### Fixed

- `wordbound.ContainsToken` no longer loops forever when `token` is empty. An empty token matches at every offset without ever advancing,
  so on any text that begins with a word character the scan spun until the process was killed. It now reports no match. This changes
  `ContainsToken("", "")` from `true` to `false`. Found by `FuzzContainsToken`; `TestContainsTokenEmptyToken` pins it.
- `egressproxy`: an upstream proxy URL whose host cannot be dialed (for example `//]`, which `url.Parse` accepts) is rejected when it is
  parsed, instead of producing an address like `]:80` that fails on every connection. Found by `FuzzParseUpstream`; the crashing input is
  committed under `egressproxy/testdata/fuzz/` and replayed by `go test`.
- **BREAKING (behavioural):** `validate`'s `pattern=` tag now requires the whole value to match, as its documentation has said since the file was created ("anchored").
  It was applied with an unanchored match, so `pattern=[A-Z]{2}[0-9]{3}` accepted `AA0000` and anything that merely contained a match (`<script>AA123`), and an alternation
  such as `pattern=a|b` accepted `abc`. The tag is now wrapped in `^(?:...)$`: one that already has `^...$` is unaffected, one that relied on substring matching starts
  rejecting. No `pattern=` tag exists in any local repository outside go-common's own tests. Found by `FuzzBindBytes` in the first real CI run of the fuzz step on a later
  PR in this series; the crashing input is committed under `validate/testdata/fuzz/` and replayed by `go test`.
  - summary: `validate`'s `pattern=` tag now requires the whole value to match (it was applied unanchored): a tag that relied on substring matching starts rejecting.

### Added

- CI (`.woodpecker.yml`): `go test -race`, `golangci-lint` (new findings only, `.golangci.yml`), `govulncheck`, an exported-API compatibility
  check (`scripts/apidiff-check.sh`), a 5 s fuzz smoke run on pull requests, and a package-index check. The existing `validate` step is unchanged
  apart from the index check. Tool versions are pinned in `scripts/tool-versions.sh`; the lint image is pinned by digest. On a pull request the lint
  and API checks compare with the merge base, not the target branch tip, so a main that moved on is not read as the PR removing its additions. The pipeline takes a full
  clone (Woodpecker's default is shallow and treeless, which has neither `HEAD^` nor a merge base) and runs for pull requests and pushes to `main` only, so a change is built once,
  not once as a push and again as a pull request. The vuln step is pinned to `golang:1.26.9`: the agents hold different 1.26 patches and govulncheck's standard-library findings
  differ between them. The pipeline passes `woodpecker-cli lint` for server v3.17.0.
  - `scripts/fuzz.sh` retries a target whose run ended in the Go fuzz engine's own `context deadline exceeded` with no failing input (seen on roughly one target in ten
    in CI: the engine's timer fires while a worker is still busy), up to three attempts. A run that wrote a failing input is a real finding and is never retried.
    `scripts/fuzz-selftest.sh` checks both paths with a fake `go`.
  - `scripts/vuln-check.sh` runs `govulncheck` on a supported Go (`golang:1.26`) and fails on any reachable vulnerability that is not in
    `scripts/govulncheck-allow.txt`, or whose entry there has expired. It starts with four entries: GO-2026-6603, -6611, -6612 and -6617, HTTP/2
    issues reachable through `golang.org/x/net@v0.58.0`. The fix is x/net v0.60.0, which requires go 1.26.0 (so did v0.59.0 and later); taking it raises
    the minimum Go version of every consumer, which is a fleet decision and not part of this change. The entries expire on 2026-11-30.
    Separately, on Go 1.25.14, the toolchain CI used until now, govulncheck reports 11 reachable standard-library vulnerabilities whose fixes exist
    only in Go 1.26.9.
- `docs/api-policy.md`: what a release promises, when a change must be marked **BREAKING:**, and the deprecation rules. The `apidiff` gate fails an
  incompatible exported-API change unless this change's `CHANGELOG.md` entry says BREAKING.
- 17 fuzz targets in `dataformat`, `jsbundle`, `pagination`, `validate`, `wordbound`, `safehttp`, `egressproxy` and `policyeval`, and
  `scripts/fuzz.sh [fuzztime]` to run them all. `FuzzIsBlocked` checks `safehttp.IsBlocked` against an independent list of the documented ranges,
  including IPv4-mapped IPv6 forms; it found nothing in a 15 s run per target.
- `lint/golangci.yml`: a shared golangci-lint v2 config for services (forbids `http.Get`/`DefaultClient`/`ListenAndServe`, adds `noctx` and
  `bodyclose`, and keeps staticcheck's SA1019 so deprecations reach consumers). Use it from the pinned module, see `lint/README.md`.
  `lint/selftest.sh` and the `lint/fixture` module keep the rules honest.
- `README.md` with a "which package do I use?" table, `docs/PACKAGES.md` generated from the packages' doc comments by
  `go run ./scripts/pkgindex` (CI fails when it is stale or a package has no comment), and runnable `Example` functions for `errors`, `env`, `ua`,
  `cache`, `clock`, `retry`, `nullify`, `wordbound`, `header`, `response`, `pagination`, `validate`, `safehttp` and `server`.
- `safehttp` package doc comment (`safehttp/doc.go`); it had none.

## [0.102.68] - 2026-10-17

### Fixed

- The `graph` tests no longer talk to the production collector. Six tests (and two more through `setGraphEnv`) configured
  `https://fleet-graph.0exec.com` with a fake writer key, and the "fully configured, valid id" case really flushed an event on
  `Shutdown`: every go-common test run on a builder, CI worker or laptop sent `POST /events` as `domain-example` and got 401
  (38 in 10 hours on 2026-10-17, from five hosts, which looked like a fleet writer without a valid key). They use a loopback
  address that refuses the connection (`unreachableCollectorURL`). Test-only; no change to the package.

## [0.102.67] - 2026-10-17

### Fixed

- `proxysupplier` `webshare_direct`: a throttled catalog page is retried on its own instead of restarting the whole
  refresh. 0.102.65 retried a failed refresh with backoff, but the catalog is paginated (about 18 requests for 1,800
  entries) and any 429 aborted the refresh, so under a throttling API most attempts died part-way and restarted from
  page 1: during the 2026-10-17 rollouts replicas needed 2.5 to 6 minutes and up to 11 whole attempts before the pool
  filled (egress failed closed meanwhile). Now each page is tried up to 6 times with the same jittered backoff and the
  API's `Retry-After` (transport errors, 429 and 5xx; a 401/403/404 is not repeated), keeping the pages already fetched;
  the refresh only fails when a page keeps failing, and then `refreshLoop` backs off as before.

## [0.102.66] - 2026-10-17

### Fixed

- `chromeguard`: the raise of a renderer's `oom_score_adj` is no longer attempted only once per process. Chromium writes its
  own score (300) into a renderer shortly after launching it and again when it reclassifies it, so a raise that came
  first was lost (seen as a flaky test with a fast scan interval, and a risk in production whenever Chromium reclassifies a
  tab). Every pass now reads each renderer, GPU and utility process's score and raises it again if something lowered it
  (one file read per target, a write only when needed). A process whose write failed for a reason other than exiting is
  still not retried on every pass. Nothing else changes; `AdjustRenderers` is still idempotent.

## [0.102.65] - 2026-10-17

### Fixed

- `proxysupplier` `webshare_direct`: a failed pool fetch is retried with backoff instead of after a whole refresh interval.
  The initial fetch ran once and, when it failed (network error, 429, 5xx, an undecodable body or zero valid entries), the
  pool stayed empty until the 10-minute ticker fired. Seen live on 2026-10-17: five go-js-proxy-network replicas restarted
  within three minutes, the Webshare account API throttled the last one, and it answered `503 egress_unavailable` (the
  egress proxy correctly failing closed) from 12:37 to 12:48. Now a failed fetch is retried after about 1 s, doubling to a
  1 minute cap, with half-random jitter so replicas that failed together do not retry together, never later than the
  refresh interval, and not sooner than the API's own `Retry-After` (seconds or HTTP date, capped at 5 minutes). A failed
  refresh with a live pool keeps serving the previous list and retries on the same schedule. The regular refresh interval is
  spread by +/- 10% so a fleet restarted together stops refreshing in lockstep.

### Added

- `proxysupplier.WebshareDirectPoolState` gains `RefreshSuccesses`, `RefreshFailures`, `ConsecutiveRefreshFailures` and
  `LastRefreshAgeSeconds` (-1 until the first success), exported by `promx` as
  `proxysupplier_webshare_direct_refresh_total{result="ok|error"}`,
  `proxysupplier_webshare_direct_refresh_consecutive_failures` and
  `proxysupplier_webshare_direct_last_refresh_age_seconds`. An empty pool is now visible as consecutive failures growing
  with an age of -1, instead of only as `pool_size 0`.
- The webshare_direct supplier has a `Close()` (stops the refresh goroutine) for tests and tools that build suppliers
  repeatedly; services never need it. Failed refreshes are logged for the first three in a row and then every tenth,
  recovery is always logged.

## [0.102.64] - 2026-10-17

### Added

- `egressproxy` package: a credential-injecting forward proxy on 127.0.0.1 (ephemeral port, never another interface) so a browser that cannot
  authenticate to an upstream proxy can still be forced through one. Chromium ignores credentials in `--proxy-server` and cannot answer a 407 in
  headless mode, so start it with `--proxy-server=<Proxy.URL()>` and `--proxy-bypass-list=<-loopback>` and let this proxy pick the upstream per
  connection through `proxysupplier.Supplier` (`ProxyURLForKey` when the supplier is a `KeyedSupplier` and `Config.KeyFunc` gives a key), add
  `Proxy-Authorization: Basic ...` and relay. It serves `CONNECT host:port` (tunnels spliced with half-close) and absolute-URI plain HTTP
  (hop-by-hop and `Proxy-*` headers stripped, `Expect: 100-continue` answered locally, WebSocket upgrades spliced after the upstream's 101).
  - **Fails closed.** There is no direct path to a target in the package; the only direct dial is to the upstream proxy. A supplier that returns
    "" (nothing configured, every exit IP cooling down), an unusable URL, or the proxy's own address is answered 503/502 and the target is never
    contacted. `Config.RequireUpstream` (set by `DefaultConfig`) only makes `Start` reject a nil `Supplier`; it opens no direct path.
  - **SSRF guard** before any dial (`Config.Guard`, default `DefaultGuard` built on `safehttp.GuardHost`): literal and resolved loopback,
    private, link-local (169.254.169.254), CGNAT and ULA targets and the compliance denylist get 403, honouring `SAFEHTTP_ALLOW_PRIVATE_IPS` /
    `SAFEHTTP_ALLOW_PRIVATE_HOSTS`; a name that cannot be verified gets 502. Verdicts are cached per host:port (30 s, 4096 entries, unavailable
    verdicts 2 s). The upstream resolves the target itself, so this is defence in depth, not a DNS-rebinding fix.
  - **Result feedback and retry.** If the supplier is a `ResultReporter`: `MarkResult(addr, false)` on a dial or handshake failure, a 407 or a 5xx
    answer to CONNECT, `MarkResult(addr, true)` on an established tunnel (plain HTTP: true on any non-407 answer). At most two attempts, the
    second on a different upstream when one exists, only before any byte reached the browser, both inside `DialTimeout` (the first uses at
    most 60% of it); plain HTTP retries only on a dial failure or a body-less GET/HEAD/OPTIONS. A 407 is never passed to the browser (502).
    `Config.IgnoreUpstream5xx` stops a 5xx from counting as a bad exit IP (webshare_direct puts an IP in a 3 minute cooldown on the first
    `MarkResult(false)`, so a dead *target* answered 502 would otherwise drain the pool).
  - **Limits**, all in `Config` and `ConfigFromEnv` (`EGRESSPROXY_MAX_CONNS` 256, `_DIAL_TIMEOUT` 10s, `_IDLE_TIMEOUT` 60s for a tunnel with no
    traffic in either direction, `_READ_HEADER_TIMEOUT` 15s, `_MAX_TUNNEL_BYTES` 64 MiB per direction with 0 = unlimited, `_REQUIRE_UPSTREAM`,
    `_IGNORE_UPSTREAM_5XX`, `_GUARD_CACHE_TTL`). Connections over `MaxConns` get an immediate 503. A zero `Config` literal has no byte cap:
    start from `DefaultConfig()` or `ConfigFromEnv()`.
  - Seen in a live check with Chromium 142: with `--proxy-server` set, Chromium's own background requests (component updater, `clients2.google.com`
    time sync, `www.google.com`) also go through the proxy, and a target the upstream cannot reach is a 5xx and so a bad-exit-IP verdict. Start the
    browser with `--disable-background-networking --disable-component-update --disable-sync` (or set `IgnoreUpstream5xx`).
  - **Metrics** on `Config.Registerer` (default `promx.Registry()`; collectors already registered are reused):
    `egressproxy_requests_total{kind,result}` (kind connect|http|unknown; result
    ok|guard_denied|guard_error|no_upstream|upstream_error|overload|byte_cap|bad_request), `egressproxy_bytes_total{direction}`,
    `egressproxy_active_tunnels`, `egressproxy_upstream_attempts_total{result}`, `egressproxy_request_duration_seconds{kind}`. No host, upstream
    address or credential is ever a label, and logs name only the upstream's host:port, scheme and a result (rate limited per cause).
- `chromeguard` package: keeps a Chromium process tree from taking its container down (no chromedp or browser-driver dependency; the service hands
  it the browsers' pids). Reads a proc filesystem whose root is `/proc` on Linux and none elsewhere (readers return `ErrUnsupported`, `Monitor.Run`
  idles), or any root given explicitly, which is how the tests run on a fake tree on every platform.
  - `Tree(rootPID)` / `TreeRSS(rootPID)` (and the same on `NewInspector(procRoot)`): the browser root and its descendants with pid, ppid, executable,
    args, role from `--type=` (browser, renderer, gpu, utility, zygote, crashpad, other), `RSSBytes` and, when `smaps_rollup` is readable,
    `PSSBytes`. Chromium rewrites the argument area of its child processes into one space-joined string, which is split back into arguments.
    Summed RSS double counts pages shared between processes (the Chromium binary, libraries, shared memory), so a tree's RSS total overstates it;
    PSS does not (`SumRSS`, `SumPSS`): a live Alpine Chromium 142 tree with three renderers summed to 726 MiB RSS and 142 MiB PSS.
  - `SetOOMScoreAdj(pid, adj)` raises `oom_score_adj` and never lowers it (raising is allowed unprivileged); `AdjustRenderers(rootPID, adj)` raises it
    (default 800) for renderer, gpu, utility and zygote children once per process (keyed by pid and start time, so pid reuse is a new process), so
    the kernel's OOM killer picks a renderer before the browser or the Go service.
  - `NewMonitor(MonitorConfig)` + `Run(ctx)`: each tick raises the OOM score of new children, SIGKILLs any renderer above `RendererMaxRSS` (one
    pid, re-checked right before the kill, never a watched browser, init or the calling process) and calls `OnBrowserOverLimit` once per episode
    when a browser's whole tree exceeds `BrowserMaxRSS` (the episode ends below 90% of it) so the service can drain and recycle that browser.
    `UsePSS` compares PSS instead of RSS. Metrics on `MonitorConfig.Registerer` (default `promx.Registry()`): `chromeguard_browser_rss_bytes{browser}`
    (index, at most 16), `chromeguard_renderers`, `chromeguard_renderer_kills_total`, `chromeguard_oom_adjusted_total`,
    `chromeguard_scan_errors_total`. Start from `DefaultMonitorConfig()` (5 s, renderers killed above 1 GiB, OOM 800): a bare `MonitorConfig`
    literal has `RendererMaxRSS` 0, which disables killing.
  - `Size(cgroup.Snapshot, SizingOptions) Sizing{Browsers, Tabs, Reason}`: tabs = clamp(min(CPUQuotaCores x 2.5, (MemLimit x 0.7 - 200 MiB x browsers) /
    250 MiB), 4, 40), browsers = clamp(ceil(tabs / 6), 1, 6), taking the largest tab count whose memory bill fits; a nil quota or limit falls back to
    `HostCPUs` / `HostMemBytes`, then `runtime.NumCPU()` and 4 GiB. The function is `Size` rather than `Sizing` because the result type has that name.
    `Reason` is one line for the start-up log.

## [0.102.63] - 2026-10-17

### Added

- `graph`: batches say which server/VM they come from. `FLEET_NODE` (the logical host name from the host-placement policy, e.g. `dockerhost`,
  `infer-runtime`; a lowercase DNS label, anything else is ignored with one log line) is sent as the `X-Fleet-Node` header on every batch POST
  (`graph.NodeHeader`, `graph.Node()`). Replicas of one service share a service id, so until now nothing in the Graph could attribute events to a
  host or VM. A header and not a JSON field on purpose: the collector decodes batches with `DisallowUnknownFields`, so a new field would make an
  older collector reject every batch from a newer sender, while an unknown header is ignored. The value is self-asserted diagnostic metadata,
  never an authorization input. Nothing changes for a process without `FLEET_NODE`.

## [0.102.62] - 2026-10-17

### Added

- `graph`: a process that is the collector (`fleet-graph`), the keystore (`apikey-service`), the vault front (`fleet-secrets`) or the keystore mesh (`keystore-mesh`) can never emit Fleet Graph events, whatever its environment says. The identity `Init` is given, or `GRAPH_SERVICE_ID`, naming one of them (`go-x` and `go_x` normalise to the plain id) takes the same fail-closed path as an invalid `GRAPH_SERVICE_ID`: no identity, `Record` drops and counts the event in `fleet_graph_events_dropped_invalid_identity_total`, `Enabled`/the instrumentation chokepoints return false, and the sender holds back anything it has. It is sticky for the process (a later `Init` cannot undo it), also holds when a `Record` arrives before `Init`, and logs one line, only when emission is otherwise configured. Why: the collector authenticates every writer batch through the keystore, so keystore telemetry would close keystore -> collector -> keystore; writer keys are materialised from the vault front; tier-0 services must not depend on the collector. This is the last layer behind go_fleet_runner (manifest refusal, render forces `GRAPH_ENABLED=false`) and services-registry (CI check), which hold the same four names. Tests pin the list and the name variants and cover each identity, the environment override, record-before-`Init`, stickiness, ordinary ids still emitting, and the log line; they also run under `-race` and `-shuffle`.

## [0.102.61] - 2026-10-17

### Fixed

- `promx` tests: the egress collector tests hard-coded an empty `service` label, but `NewEgressCollectors` labels with `ServiceID()`, a package
  global that `TestInitIdempotent` sets. They passed only when run before that test, so `go test -count=2 ./promx` (and any shuffled order) failed
  with `requests_total = 0, want 1`. They now read the collector's own `service` label. Test-only, no behaviour change.

## [0.102.60] - 2026-10-17

### Added

- `egress` package: one rule for which requests use the egress proxy, by destination instead of by each host's `NO_PROXY`. Our own destinations are
  always direct: the fleet apex domains and their subdomains (`0exec.com`, `0crawl.com`, `0own.com`, `0docker.com`, `0infer.com`, matched on a
  label boundary so `evil0exec.com` is not ours), private / loopback / link-local / tailnet (100.64.0.0/10) addresses, `localhost`,
  `host.docker.internal` and bare container names. Everything else follows the standard environment proxy (`http.ProxyFromEnvironment`, so
  `NO_PROXY` still applies). `FLEET_DIRECT_DOMAINS` (comma-separated, each with a dot) adds domains without a release. Exposes `egress.Direct`,
  `egress.Proxy` (an `http.Transport.Proxy` function) and `egress.Transport()` (a clone of `http.DefaultTransport` using it).

### Changed

- The `fleetfetch` cross-host cache client and `ratecoord` use the egress rule, so a call to one of our own services no longer depends on the
  host's `NO_PROXY` listing it. `safehttp` is deliberately unchanged: it is the generic public-web client, and the runner's strict egress policy
  keeps even our own public hostnames on the proxy for proxy_egress services that scan targets with it. Third-party destinations are unchanged.
- **BREAKING (only for 0.102.59):** `GRAPH_USE_EGRESS_PROXY` is removed; `graph` is always direct. Nothing used it.
  - summary: `GRAPH_USE_EGRESS_PROXY` is removed and `graph` is always direct: only a service on 0.102.59 that set it is affected (nothing used it).
- `golang.org/x/net` is an indirect dependency again (the Graph client no longer builds a proxy function from the environment).

## [0.102.59] - 2026-10-17

### Changed

- **Behaviour change:** `graph` connects to the collector directly (batches and lookups) instead of through the egress proxy, and ignores
  `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` for that traffic. This reverses the routing added in 0.102.46 (#149).
- Correction (added in 0.102.60): the failed batches seen on pool replicas (18-27%) were collector overload, not the proxy hop. The collector's
  `/brief` handler was starving `POST /events`; it was fixed in go-fleet-graph on 2026-10-16 23:38Z, after which proxy-path senders failed about
  1.4% outside short collector 503 bursts. Direct delivery is kept because the collector is one of our own services (no reason to send it through
  the external proxy), it gives the collector the real source host, and it stops spending proxy bandwidth on roughly 20k telemetry batches a day.
- 0.102.59 also added `GRAPH_USE_EGRESS_PROXY=true` to restore the old routing; 0.102.60 removes it (no host needs it: the collector is reachable
  directly from every runtime probed).

## [0.102.58] - 2026-10-16

### Fixed

- `apikey`: a hedged `Verify` can no longer spend a use of a limited key that the first attempt also spends. An attempt sent while another is
  still in flight now carries `X-Keystore-Hedge: 1` (`apikey.HedgeHeader`); go_apikey_service >= 0.7.5 answers such a check for a use-limited
  key with 409, which is not definitive here and does not count against the node's breaker, so the first attempt's answer decides. Before, the
  first node could spend the use but answer last while the second, its forwarded claim refused, answered 401 first: the caller was refused
  with the use gone. A failover after an attempt failed (nothing else in flight) is sent without the header and claims as before. Older
  keystores ignore the header.

## [0.102.57] - 2026-10-16

### Added

- `graph`: a fail-closed `GRAPH_SERVICE_ID` is now observable. When `GRAPH_SERVICE_ID` is set but is not a lowercase DNS label (`[a-z0-9-]`, at
  most 63 characters, no leading or trailing `-`; a legacy underscored package name such as `go_example_service` fails), the service still emits
  nothing, as before, but no longer silently:
  - `graph.Counters.EventsDroppedInvalidIdentity` (JSON `events_dropped_invalid_identity`) counts every event dropped for it, exported by
    `promx.AutoWire` / `promx.NewGraphCollectors` as `fleet_graph_events_dropped_invalid_identity_total` (label `service`, best-effort
    registration like the other `fleet_graph_*` collectors). It counts in `Record` and in the `RoundTripper` / `Middleware` chokepoints, which
    return before `Record` while `Enabled()` is false; `Enabled()` itself stays side-effect free. It only counts while Graph is otherwise configured
    to emit, so a service with Graph off sees 0.
  - One log line per process (standard library `log`; `graph` cannot import `server`) when `Init` or the pre-`Init` bootstrap resolves the invalid
    value: it names `GRAPH_SERVICE_ID` and says Graph emission is off. It never prints a key, a secret or the rejected value, and `Record` still
    never logs (one atomic add on the hot path).

## [0.102.56] - 2026-10-16

### Fixed

- `graph`: a service that made outbound calls (secrets lookups, a poller) before `graph.Init` / `server.New` got HTTP 403 on every Graph batch and
  never delivered an event, until restart, and again after the restart. Two causes, both fixed:
  - Events recorded while the identity was still unknown carried caller `unknown`, and the collector rejects the whole batch when one event does
    not belong to the authenticated writer. Now `GRAPH_SERVICE_ID` is resolved when the package bootstraps (before `Init`); without it, such
    events are held in the ring with no owner, the sender ships nothing until the identity is known, and the held events are stamped with it
    when they are sent (the service's own side only: caller of an outbound event, target of an inbound one). `graph.ServiceID()` returns `""`
    instead of `"unknown"` before the identity is known, and `graph.Enabled()` is true before `Init` when writer config is complete (an invalid
    `GRAPH_SERVICE_ID` still fails closed).
  - The sender kept a failed batch in `pending` and resent that exact batch on every flush, so one rejected batch blocked all later ones. A batch
    the collector refuses for its content (403, 400, 413, 415, 422) is now dropped and counted in `Counters.BatchesRejected`
    (`fleet_graph_batches_rejected_total`), and the next batch goes out on the next tick. 403 still starts the 60 s cooldown. 401 (expired key),
    408, 429, 5xx and transport errors keep the batch for a later flush, as before.

### Added

- `graph.Counters.BatchesRejected` (a subset of `BatchesFailed`) and promx collectors for the Graph sender counters, registered by `promx.AutoWire`:
  `fleet_graph_events_recorded_total`, `fleet_graph_events_dropped_total`, `fleet_graph_batches_sent_total`, `fleet_graph_batches_failed_total`,
  `fleet_graph_batches_rejected_total` (label `service`). A name a service already registers itself is skipped, not a start-up error.

## [0.102.55] - 2026-10-16

### Added

- `apikey.KeyID(key)`, `KeyMeta.KeyID` / `KeyMeta.KeyPrefix`, `IssueResult.KeyID` and `Client.RevokeID(ctx, keyID)`: the keystore's stable,
  non-secret key id (`kid_` + 24 hex of SHA-256; go_apikey_service >= 0.4.9). Keystore mesh Phase 0: callers stop needing a key's value
  back from the keystore before keys are stored hashed. `RevokeID` refuses anything that is not a `kid_` id, so a raw key is never sent as one.

### Deprecated

- `KeyMeta.Key` (the full value from `/list`): a keystore that stores keys hashed leaves it empty.

## [0.102.54] - 2026-10-16

### Added

- `apikey.Client` can talk to several keystore nodes (keystore mesh, Phase 0: claude_team/KEYSTORE-MESH-PLAN-2026-10-15.md). `Endpoints` /
  `APIKEY_SERVICE_URLS` is an ordered list, the node on the caller's own host first. `Verify` asks the first healthy endpoint and, if it has
  not answered within `HedgeDelay` (default 300 ms, `APIKEY_SERVICE_HEDGE_MS`), also asks the next; the first definitive answer wins. A
  refused connection, a transport error or a 5xx moves on at once; a 401 is definitive and is never re-asked elsewhere. A per-endpoint
  breaker (3 consecutive failures, 30 s) moves a failing node to the back of the order without dropping it, and a request the client
  cancelled itself (because another node answered first) never counts against a node. Environment URLs are accepted only as plain http to a
  loopback, RFC 1918, tailnet (100.64/10), `host.docker.internal` or single-label container address, or https to the fleet's own domains,
  with no credentials, path, query or fragment; anything else is dropped and logged, so a presented key is never sent elsewhere.
- Admin calls (`Issue`, `Revoke`, `List`, `ListExact`, `Purge`) move to the next endpoint after a transport error. `Issue` moves on only when
  the connection was refused (the request never reached a node), so it can never mint two keys; any HTTP answer is final.
- `VerifyObserver` / `VerifyEvent` (endpoint used, result, attempts, hedged, duration) for metrics.

With one endpoint (no `APIKEY_SERVICE_URLS`) behaviour is unchanged: one request, the same errors.

## [0.102.53] - 2026-10-13

### Fixed

- Instrument `proxysupplier.HTTPClient` with the Fleet Graph observer so outbound requests using rotating proxy suppliers are recorded like `safehttp.NewClient` requests.

## [0.102.52] - 2026-10-13

### Added

- `secrets.Client` fails soft, so a service can start while the primary vault host is down (Tier-0 resilience, step 5).
  `WithFallbacks(urls...)` / `FLEET_SECRETS_FALLBACK_URLS` is an ordered list of extra vault base URLs (a follower on another site),
  tried after the primary on an outage-type failure (transport error, 5xx, 429, undecodable or empty answer). Environment URLs must be
  https on an approved `fleet-secrets*` host (0exec/0mcp/0own/0docker); anything else is dropped so the API key never travels there.
  `WithCache(dir, maxAge)` / `FLEET_SECRETS_CACHE_DIR` + `FLEET_SECRETS_CACHE_MAX_AGE` (default 168h) keeps an encrypted last-known-good copy
  (AES-256-GCM under a key derived from the caller's own API key, 0600 files in a 0700 directory) and serves it only when every URL failed.
  401/403/404 are definitive: never retried elsewhere and never answered from the cache, so a revoked key or a deleted secret is not resurrected.
- `GetDetailed` returns provenance (`live`, `alt`, `cache`, plus URL and cache age) without exposing the value in logs.
- `NewFromEnv`, the telemetry OpenObserve token read and `apikey.ResolveAdminToken` pick the new variables up through `WithEnv`;
  with none set, behaviour is unchanged.

## [0.102.51] - 2026-10-13

### Fixed

- Make `safehttp.GuardHost` archive-aware so Common Crawl requests skip live DNS
  resolution while still rejecting private IPs, localhost, local suffixes, and
  the fleet compliance denylist.
- Apply `FLEET_FETCH_SOURCE=commoncrawl` as the default archive-only requirement
  in the server middleware. An explicit `source=live` request still overrides
  that process default.


## [0.102.50] - 2026-10-12

### Fixed

- Common Crawl archive requests skip live DNS resolution during URL validation,
  while private IP literals remain blocked. Archive `HEAD` probes now read the
  captured response through Fetch Cache and suppress its body, avoiding live
  origin egress.

## [0.102.49] - 2026-10-12

### Fixed

- Honor an explicit Common Crawl source even when a service's `safehttp` client
  normally opts out of Fleet Fetch Cache. Archive GETs fail closed with 502 when
  the cache is unavailable instead of silently fetching the live origin. Keep
  live probes and the fetch-cache loop guard on their existing direct paths.
  Verified with `go test ./...`, `go vet ./...`, and `git diff --check`.

## [0.102.48] - 2026-10-12

### Fixed

- `telemetry.Init` no longer leaves tracing off for the life of the process when
  the one-shot read of the OpenObserve ingestion credential fails transiently.
  v0.102.47 deliberately kept a single 2 s attempt, and a direct read on a
  loaded host was measured once at 2.1 s: enough to disable tracing until the
  next restart. A transient failure (timeout, connection error, HTTP 5xx, 408
  or 429) now starts one bounded background retry that installs the tracer
  provider when the credential is finally read.
  - `Init` is unchanged for the caller: the same single attempt with the same
    2 s limit, then it returns. It never waits for the retry.
  - Bounded: at most 6 further attempts after waits of 2, 4, 8, 16, 32 and 64 s
    (about two minutes in all; each wait is shortened by up to 20% so services
    restarting together do not meet the vault in lockstep), every attempt with
    the same 2 s limit, so the worst case is about 2.3 minutes. One retry runs
    per process: a later `Init` shares it, and it stands down without another
    vault read if a later `Init` has already brought tracing up.
  - Only transient failures are retried. A 401, 403 or 404, and every other
    answer the vault gave on purpose (any other 4xx, a redirect, an empty
    value, an unusable body), is a configuration error: it is reported once,
    exactly as before, and is never retried. A retry that meets one stops at
    once.
  - Every attempt repeats exactly the read that failed: the same validated
    vault URL and API key (the environment and key files are not read again),
    the same direct, redirect-free client, the same 2 s limit. A success goes
    through the same credential, endpoint and provider code as a start-time
    success, and the approved-host checks still decide whether any read or
    install can happen. Neither the API key nor the credential can reach a log
    line or an error: failure reasons come from a fixed vocabulary (`timeout`,
    `connection error`, `HTTP 503`, ...), never from error text.
  - `Config.Shutdown` cancels the retry, aborts an attempt in flight and waits
    for the goroutine, so no provider is installed after `Shutdown` returns; a
    provider the retry installed earlier is shut down like any other.
  - One line is logged when tracing comes up late (`telemetry: OpenObserve
    credential loaded on retry N of 6, ...; tracing is now enabled`) and one
    error line, through the OpenTelemetry error handler like the start-time
    failure, when the retry gives up (`... still unavailable after 6 retries
    ...` or `... non-retryable error on retry N (HTTP 403) ...`). A retry
    cancelled by `Shutdown`, or made redundant by another `Init`, is silent.
  - Behaviour change for pinned endpoints: when `OTEL_EXPORTER_OTLP_ENDPOINT`
    names an approved OTLP host and the credential read fails transiently,
    `Init` no longer installs a provider that exports without credentials (it
    could never authenticate, and it would occupy the process's one provider);
    it waits for the retry. A non-transient failure is handled exactly as
    before.
  - Verification: tests with a fake vault that fails N times then succeeds,
    that never succeeds, and that answers 401, 403 and 404 (plus a retry that
    meets a 403); Shutdown while waiting, during an attempt, and racing the
    install; shared and stand-down behaviour across `Init` calls; the real
    `readOpenObserveToken` against timeouts, refused connections, 5xx and
    redirects. They fail against a build with the retry disabled (negative
    control) and pass with it; `gofmt`, `go vet`, `go build` and
    `go test ./...` pass, and the telemetry tests were run repeatedly under
    `go test -race` with several `GOMAXPROCS` values.

### Added

- `secrets.StatusError`: `Client.Get` returns it for a non-200 status, carrying
  the secret name and the numeric status and never the response body, so a
  caller can tell an outage worth retrying (5xx) from a configuration error
  (401, 403, 404) with `errors.As` instead of parsing text. The error message is
  unchanged.

## [0.102.47] - 2026-10-12

### Fixed

- Reach the Fleet Secrets vault and the fleet's approved OpenObserve OTLP hosts
  directly, never through `HTTP_PROXY`/`HTTPS_PROXY`. A `proxy_egress` service
  has `HTTPS_PROXY` pointing at an external scraping proxy, and the fleet's
  strict service-scoped bundles remove the public mesh suffixes from
  `NO_PROXY`; the one-shot OpenObserve credential read in `telemetry.Init` was
  therefore tunnelled through that proxy, arrived at the gateway from a public
  address, and was refused by the vault vhost's source allowlist (403). The
  failure was identical on every start, so affected services logged
  `telemetry: could not load OpenObserve ingestion credential from fleet
  secrets` and never exported traces. Observed live on both `go-proxy`
  replicas (same image digest, different hosts, every restart); the same
  credential read succeeds with HTTP 200 on the direct path.
  - New `secrets.DirectTransport()` (a clone of `http.DefaultTransport` with the
    proxy cleared) is used for the telemetry vault read and for the default
    client of `secrets.NewFromEnv`, whose host is already pinned to
    `fleet-secrets.0exec.com`. A caller-supplied `Doer` is used unchanged.
  - The OTLP trace exporter opts out of the environment proxy only for the
    approved hosts (`otlp.0exec.com`, `openobserve.0own.com`); a custom
    `OTEL_EXPORTER_OTLP_ENDPOINT` keeps standard OpenTelemetry proxy behaviour.
  - Unchanged: the 2 s vault read timeout, redirect refusal, the approved-host
    checks, and the fail-closed behaviour when the credential cannot be loaded.
  - Verification: new tests fail against the old behaviour (negative control)
    and pass with the fix; `gofmt`, `go vet`, `go build` and `go test ./...`
    pass.
  - Not changed here: the credential read is still a single 2 s attempt at
    process start. A saturated host can exceed it (a direct read measured 2.1 s
    on a loaded host), which also leaves telemetry off until the next restart; a
    bounded background retry is a separate follow-up.

## [0.102.46] - 2026-10-12

### Fixed

- Route Fleet Graph writes and lookups through the configured HTTPS egress proxy,
  even when the fleet-wide `NO_PROXY` suffix matches `.0exec.com`. Keep loopback
  development collectors direct and retain the existing no-redirect credential
  boundary.


## [0.102.45] - 2026-10-12

### Added

- Add the opt-in `bootgate` package: `Wait`/`Go` spread heavy boot-time work
  with a deterministic per-host (FNV-1a of hostname) jitter and back off while
  the host 1-minute load average is above a ceiling, bounded by a max wait.
  Configured via `BOOT_BASE_DELAY`, `BOOT_JITTER_MAX`, `BOOT_MAX_LOAD`,
  `BOOT_MAX_WAIT`, `BOOT_POLL_EVERY`; everything defaults to disabled, so
  nothing changes until a service calls `Wait` and an operator sets env.

## [0.102.44] - 2026-10-12

### Fixed

- Allow the Common Crawl staging profile's local probe key only when both the
  service environment is staging and the fetch source is Common Crawl. Keep
  production and live-mode requests keystore-gated.

## [0.102.43] - 2026-10-11

### Added

- Add a validated, context-scoped logical target override for Fleet Graph so
  calls through shared internal addresses retain the canonical service ID.
- Bound Fleet Graph shutdown flushes by the caller context and wire the shared
  server lifecycle to perform that bounded flush.

## [0.102.42] - 2026-10-11

### Fixed

- Allow the fleet-rendered `GRAPH_SERVICE_ID` to bind Graph event batches to
  the canonical registry identity instead of a legacy Go package name. Invalid
  overrides fail closed without affecting application requests.

## [0.102.41] - 2026-10-10

### Fixed

- Keep Common Crawl requests archive-only when the fetch-cache delegate fails;
  live requests retain their existing direct-egress fallback.

## [0.102.40] - 2026-10-09

### Added

- Carry request-level source selection through standard Go Common servers into
  fleetfetch. Requests can choose live or archive-only Common Crawl without
  changing process defaults; explicitly pinned clients retain precedence.

## [0.102.39] - 2026-10-09

### Added

- Start the opt-in Pyroscope profiler automatically from the shared server
  lifecycle, using the stable public service slug and stopping it on shutdown.
  Keep explicit `profiling.StartFromEnv` callers compatible through one
  idempotent process-wide profiler.

## [0.102.38] - 2026-10-09

### Added

- Allow opt-in Pyroscope Basic Auth credentials to be fetched by service-scoped
  Fleet Secrets names, using the shared protected-key client and approved HTTPS
  endpoint. Keep mounted credential files supported.

## [0.102.37] - 2026-10-09

### Added

- Add `fleetfetch.WithMaxBodyBytes` to bound memory for cached and direct
  response reads.

## [0.102.36] - 2026-10-09

### Added

- Add opt-in continuous Go profiling through the official Pyroscope SDK, with
  bounded profiles, TLS and file-mounted credentials for remote endpoints.

## [0.102.35] - 2026-10-09

### Added

- Add current-cgroup CPU and memory snapshots, quota, throttling, and PSI pressure metrics. Auto-wire them for standard Go Common servers and allow custom Prometheus registries to opt in.

## [0.102.34] - 2026-10-08

### Fixed

- Accept `FLEET_FETCH_SOURCE=live` (case-insensitively and with surrounding
  whitespace) as the default live-origin mode, matching the documented
  environment contract.

## [0.102.33] - 2026-10-08

### Fixed

- Send default Go Common OTLP traces to the OpenObserve instance on 0own.
  Keep automatic vault credentials restricted to the approved HTTPS endpoints.

## [0.102.32] - 2026-10-08

### Added

- Add lease-bound SPIFFE client operations for issuing and revoking short-lived
  registry pull credentials; keep bearer tokens redacted from formatting and
  clear caller-owned token buffers after use.

## [0.102.31] - 2026-10-08

### Added

- Allow `FLEET_FETCH_SOURCE=commoncrawl` to select archive-only fetches for
  fleetfetch clients by default; explicit client options take precedence.

## [0.102.30] - 2026-10-08

### Added

- Add `fleetfetch.WithSource(fleetfetch.SourceCommonCrawl)` and
  `Client.GetFromSource` for archive-backed URL fetches. Common Crawl selection
  is independent from render mode and never falls back to a live origin when
  the fetch cache is unavailable.

## [0.102.29] - 2026-10-07

### Added

- Support protected file-mounted fetch-cache API keys and route explicit HTTPS
  cache endpoints through the configured egress proxy while keeping the
  Docker-network default on direct container DNS.

## [0.102.28] - 2026-10-07

### Fixed

- Stop Go Common server entrypoints and internal clients from inventing a
  shared public fallback credential. Require configured service keys for
  fetch-cache and pentest findings-store calls; reject the retired value even
  when it remains in a legacy local-token list.
- Remove stale auth examples that put credentials in request URLs or path
  prefixes.


## [0.102.27] - 2026-10-07

### Added

- Add an explicit single-replica `recreate` deployment intent strategy with a
  required one-replica interruption budget, so Compose replacement downtime is
  represented honestly instead of being mislabeled as a rolling update.

## [0.102.26] - 2026-10-07

### Added

- Allow SPIFFE mTLS HTTP clients to set a bounded total and response-header
  timeout; deployment authority calls use a 160-second default for evidence
  evaluation while lease actions remain caller-context bounded.

## [0.102.25] - 2026-10-07

### Added

- Add the shared SPIFFE mTLS deployment-authority client for verifying signed
  authorization decisions and acquiring, validating, renewing, and finishing
  durable target leases.
- Consolidate duplicate-key, unknown-field, and trailing-value JSON rejection
  for security-sensitive request and evidence decoders.

## [0.102.24] - 2026-10-07

### Fixed

- Accept bounded URI-fragment characters in signed provenance identity fields,
  preserving the exact SLSA builder identity used by `publish-image`.

## [0.102.23] - 2026-10-07

### Added

- Share the strict v1 DSSE deployment-authorization statement, Ed25519 signer,
  and verifier between the deployment authority and fleet runner.

## [0.102.22] - 2026-10-07

### Added

- Add a shared, strict v1 deployment-intent codec and canonical digest for
  control-plane services and the fleet runner.

## [0.102.21] - 2026-10-06

### Fixed

- Allow an explicit empty provider-issuer map for default-deny credential
  broker policies while rejecting every grant without a configured issuer.

## [0.102.20] - 2026-10-06

### Added

- Add SPIRE Workload API adapters for single-audience JWT-SVIDs and rotating
  X.509-SVID mTLS clients with exact peer-ID allowlists.
- Allow credential task clients to use SPIFFE mTLS without a bootstrap API key
  while retaining broker-signed task proofs for task close and lease requests.

## [0.102.19] - 2026-10-06

### Fixed

- Reload file-backed Fleet Graph writer credentials for each batch, allowing
  atomic secret rotation without restarting the service. A configured but
  unavailable key file continues to fail closed, and the sender remains ready
  to resume when the file becomes available.

## [0.102.18] - 2026-10-06

### Fixed

- Route default OTLP exports through `otlp.0exec.com` and attach the
  service-scoped OpenObserve token to that approved ingress. Preserve the
  current `openobserve.0own.com` route during migration while rejecting the
  retired 0docker endpoint.

## [0.102.17] - 2026-10-06

### Fixed

- Send Go Common traces to OpenObserve on 0own and support a protected,
  file-backed ingestion token so enrolled services do not need runtime access
  to Fleet Secrets.

## [0.102.16] - 2026-10-02

### Added

- Read the Fleet Graph writer credential from `GRAPH_API_KEY_FILE`, with
  fail-closed file precedence and the existing environment variable retained
  as a migration fallback.

## [0.102.15] - 2026-10-02

### Fixed

- Record internal Fleet Fetch Cache requests in Fleet Graph without sending
  Docker-DNS traffic through the environment proxy, and resolve `go_<name>`
  container hostnames to canonical service slugs.
- Make the findings-store outage test deterministic instead of assuming a
  particular loopback port is unused on CI agents.

## [0.102.14] - 2026-10-02

### Fixed

- Bound provider-issued credential expiry from the time the issuer returns the
  credential, avoiding false lease failures caused by provider latency or
  whole-second expiry timestamps while preserving the requested TTL limit.

## [0.102.13] - 2026-10-01

### Fixed

- Authenticate every task-bound broker request at the fleet gateway with the
  workload's service-scoped broker key, including acquire, revoke, and task
  cleanup. The task-bound client inherits the key from its TaskManager and
  never sends it to the credential target.

## [0.102.12] - 2026-10-01

### Added

- Add an HTTPS-only task manager that asks the broker to generate task IDs and
  task proofs after validating the workload's bootstrap service credential.
  Bind lease clients to that broker-created task and close malformed task
  responses when enough proof is available for cleanup.
- Bind every lease to one canonical target path and an explicit set of HTTP
  methods; reject query strings and any use outside that exact method/path
  scope before attaching the credential.
- Harden task proof redaction, redirect handling, response bounds, and task
  lifecycle coverage.

## [0.102.11] - 2026-10-01

### Added

- Add the callback-scoped credential lease client and fail-closed broker HTTP
  core with task-bound authorization, policy checks, atomic idempotency hooks,
  protected audit events, and provider issue/revoke interfaces.

## [0.102.10] - 2026-10-01

### Fixed

- Keep the graph sender batch identity synchronized when `Init` changes a
  service from its package name to its canonical fleet identity. Protect the
  sender identity snapshot and package identity reads against concurrent
  updates.

## [0.102.9] - 2026-09-30

### Added

- Read the scoped Fleet Secrets bootstrap API key from a protected file, allowing
  deployments to keep it out of Docker Compose environment files.
- Reject key files that are missing, empty, non-regular, or group/world readable.


## [0.102.8] - 2026-09-30

### Fixed

- Keep an explicit OTLP endpoint while loading the per-service OpenObserve
  ingestion token from Fleet Secrets for approved OpenObserve hostnames. Never
  forward the vault token to arbitrary collector hosts.

### Changed

- Use the stable `https://otlp.0exec.com/api/default/v1/traces` endpoint by
  default so backend moves do not require application changes.

## [0.102.7] - 2026-09-29

### Added

- Add privacy-safe verified caller attributes to active inbound OpenTelemetry
  spans so traces can be grouped by fleet service identity.

## [0.102.6] - 2026-09-29

### Fixed

- Prefer FLEET_SECRETS_API_KEY for OpenObserve token lookup so services can
  scope vault access without widening their application API key. Retain
  FLEET_API_KEY as a compatibility fallback.


## [0.102.5] - 2026-09-28

### Added

- Enable direct OpenObserve OTLP trace export for services with a scoped
  `FLEET_API_KEY`, loading an ingestion-only token from Go Fleet Secrets over
  verified HTTPS. Export stays disabled when the key or allowlisted secret is
  unavailable; W3C propagation continues without blocking service startup.

## [0.102.4] - 2026-09-28

### Fixed

- Stop deriving inbound runtime-graph callers from spoofable User-Agent and
  X-Fleet-Caller headers. Keystore-authenticated service principals are carried
  in request context; other inbound requests are recorded as unknown.
- Run graph observation inside authentication middleware so inbound topology
  uses only identities established by the trusted gateway or keystore.
- For custom HTTP servers, accept `X-Auth-User` as an inbound graph caller only
  when the TCP peer matches a literal IP in `GRAPH_TRUSTED_CALLER_IPS`.

## [0.102.3] - 2026-09-27

### Added

- Add privacy-safe pgx spans for batch, COPY, and prepare operations, plus a
  validated `dbx.CopyFrom` helper for PostgreSQL binary COPY.

### Fixed

- Preserve any application-configured pgx tracer alongside Go Common telemetry.
- Revalidate identifier parts at query-build time before composing SQL.

## [0.102.2] - 2026-09-27

### Fixed

- Update the gRPC runtime to v1.83.2 to address the OSV-reported high-severity
  vulnerability in v1.83.1.

## [0.102.1] - 2026-09-27

### Fixed

- Return the telemetry HTTP RoundTripper as a pointer so transport-chain
  introspection can traverse it and find the underlying configured transport.

## [0.102.0] - 2026-09-27

### Added

- Add fleet-wide OpenTelemetry tracing to `server.New` and `safehttp.NewClient`,
  with W3C trace-context propagation, HTTPS-first OTLP export, parent-based
  sampling, graceful exporter shutdown, privacy-safe HTTP spans, and Prometheus
  request-duration exemplars keyed by sampled trace ID.
- Add `dbx` database primitives for instrumented `database/sql` and native
  PostgreSQL `pgxpool` access, plus validated-identifier and parameterized CRUD
  builders that require filters for updates and deletes. `database/sql` pools
  have finite defaults for open/idle connections and connection lifetime.

## [0.101.3] - 2026-09-23

### Fixed

- Preserve an explicit Webshare target-policy denial of an HTTPS proxy
  CONNECT as unreachable, rather than letting a preceding fetch-cache 502
  classify it as an upstream service failure. Generic cache 5xx responses
  remain errors.

## [0.101.1] - 2026-09-21

### Added

- `server.WithKeystoreAuthTierTrustedProxyNoCache`, an opt-in direct-key
  authentication path for consumable credentials. It preserves the existing
  trusted-proxy and tier checks while ensuring every direct request reaches
  the authoritative key verifier instead of a positive credential cache.

### Fixed

- Make the corpus-outage test explicitly bypass `HTTP_PROXY`, so it continues
  to verify a refused loopback connection rather than a proxy-generated HTTP
  response in CI.

## [0.101.0] - 2026-09-20

### Added

- `proxysupplier`: opt-in sticky IP selection for `webshare_direct`.
  `KeyedSupplier.ProxyURLForKey(key)` deterministically selects the same
  proxy entry for the same key (cooldown-aware fallback), without
  touching `ProxyURL()`'s round-robin cursor. `HTTPClient` consults it
  automatically via the new `StickyKeyHeader` request header — set it on
  one outbound request to pin that request to a consistent exit IP,
  leave it off for the existing default round-robin behavior. Does not
  change connection reuse: `webshare_direct` still forces a fresh TCP
  connection per request, only which exit IP a request lands on is
  pinned.

## [0.100.1] - 2026-09-20

- Restore the documented fleet probe contract: `/selftest`, like `/health`
  and `/version`, bypasses API-key middleware. This keeps deploy smoke probes
  available without reintroducing a shared local-token bypass for application
  routes.

## [0.100.0] - 2026-09-20

- Add an opt-in trusted-proxy CIDR guard for keystore gateway headers, so
  direct private-network callers cannot forge `X-Auth-User` or `X-Auth-Tier`.
- Add `server.WithKeystoreAuthTierTrustedProxy` for security-sensitive
  tier-gated services while preserving existing service behavior by default.

All notable changes to `github.com/baditaflorin/go-common` are recorded here.
Versioning follows semver on the git-tag axis; the package itself has no
embedded version string (consumers pin via `go.mod`).

## v0.99.1 - 2026-09-20

### Fixed

- `safehttp` byte-accuracy: `responseBytes()` only ever checked
  `resp.ContentLength`, silently reporting 0 for any chunked/gzip'd
  response with no Content-Length header (most dynamic HTML). A new
  `countingBody` wrapper measures bytes the caller actually reads instead
  — the `EgressObserver` event for a request with a response body now
  fires when the body is `Close()`'d rather than synchronously in
  `RoundTrip`, so `Bytes` reflects real consumption. Falls back to
  Content-Length only when the caller closes without reading at all (a
  legitimate status-only check, not "zero bytes transferred").

### Added

- `EgressEvent.Channel` ("direct"/"proxy"/"cache_hit"), wired into
  `promx.EgressCollectors`' `bytesTotal` vector — a fetch-cache hit no
  longer looks like real network spend, and (previously unobserved
  entirely) cache-hit responses now emit an event with an exact byte
  count.

## v0.99.0 - 2026-09-15

### Added

- New `qrterm` package: turns short content (a URL, an id, a token) into
  a QR code — half-block terminal art via `TextString`/`Write` with
  `FormatText`, or `image/png` bytes via `PNG`/`Write` with `FormatPNG`.
  `QueryFormat(r)` parses the conventional `?qr` / `?qr=1` / `?qr=png`
  query parameter into a format string, matching the ad-hoc
  implementation `go-fleet-pipe` had carried in its own `qr.go` since
  its `?qr` feature shipped. Extracted so every fleet service can adopt
  the same "push from a headless box, scan the code off your terminal"
  pattern without re-hand-rolling it — `go-fleet-pipe` is migrated to
  this package as of its own next release. `Write` returns an error
  instead of writing an HTTP error response itself, so each service
  keeps using its own `httpError` shape. Adds
  `github.com/skip2/go-qrcode` as a new `go-common` dependency.

## v0.98.1 - 2026-09-11

### Fixed

- Graph middleware now excludes only the collector's own POST /events
  ingest batch. This pairs with the existing outbound collector guard, so the
  collector can safely observe its normal reader surface without creating an
  endless self-ingest feedback loop.

## v0.98.0 - 2026-09-08

### Added

- Graph outbound observation now supports the opt-in
  `GRAPH_TARGET_ALIASES` mapping for explicitly configured, bare internal
  `go-fleet-*` Docker hostnames. Entries use
  `host=canonical-service-id` form and are strictly validated before use.
  This preserves the caller's actual HTTP transport and turns known
  docker-network calls such as `go-fleet-dns-sync` into the same canonical
  graph edge (`fleet-dns-sync`) used by registry-declared dependencies.
  Invalid or unset aliases leave the existing external-host classification
  unchanged.

## v0.97.0 — 2026-09-02

### Added

- `apikey.Client.ListExact(user, scope, tier)` performs the keystore's narrow
  exact-identity list query. It requires all three selectors and surfaces the
  server's truncation indicator so revocation workflows can fail closed rather
  than confuse a capped global inventory with a complete match set.

## v0.95.0 — 2026-08-28

### Added

- `proxysupplier` now publishes live `webshare_direct` pool gauges on the
  existing Prometheus `/metrics` surface:
  `proxysupplier_webshare_direct_pool_size`,
  `proxysupplier_webshare_direct_pool_in_cooldown`, and
  `proxysupplier_webshare_direct_pool_eligible`, each labelled by service.
  The gauges take one consistent pool snapshot at scrape time, reflecting
  natural cooldown expiry without adding work to the per-request
  `ProxyURL` or `MarkResult` paths.

## v0.94.0 — 2026-08-26

### Fixed

- `proxysupplier.HTTPClient` now supports the asynchronous
  `webshare_direct` supplier during its initial proxy-pool refresh. It no
  longer returns a permanently nil client merely because no proxy URL is
  available at construction time; the transport resolves the current proxy
  URL per request instead. This prevents dependent services from panicking
  on their first live outbound request after startup.

## v0.90.0 — 2026-08-24

### Added

- `safehttp.IsBlockedForHost(host, ip)` + `SAFEHTTP_ALLOW_PRIVATE_HOSTS`
  env var (`SetAllowedPrivateHosts` for tests/runtime overrides): a
  hostname-scoped narrowing of the existing `SAFEHTTP_ALLOW_PRIVATE_IPS`
  escape hatch.
  - **Problem:** `SAFEHTTP_ALLOW_PRIVATE_IPS` bypasses the SSRF guard for
    an IP, full stop — ANY hostname that happens to resolve to that IP
    gets through, not just the intended target. On a fleet whose
    split-horizon internal DNS rewrites both the operator's own public
    domains AND every sibling fleet service's `*.0exec.com`/`*.0crawl.com`
    hostname to the same internal gateway IP (to skip a public NAT
    hairpin), allowlisting that one IP for a service that fetches
    ARBITRARY caller-supplied URLs (a URL-fetch cache/analyzer) would
    let any caller pivot to sibling services sharing that gateway
    address — including ones with no protection beyond "must originate
    from inside the fleet LAN" (the IP-based bypass makes the fetcher
    look like it's inside the LAN). Confirmed concretely exploitable
    against at least one real internal-only, `auth: none` fleet service
    during the investigation that motivated this change.
  - **Fix:** `IsBlockedForHost(host, ip)` requires BOTH
    `SAFEHTTP_ALLOW_PRIVATE_IPS` (the IP) AND `SAFEHTTP_ALLOW_PRIVATE_HOSTS`
    (the ORIGINAL hostname, suffix-matched so subdomains are included) to
    independently allow the pair before bypassing the private-range
    check. `GuardHost` and the dialer's `Dialer.Control` DNS-rebind
    re-check (`makeDialer`) both now call `IsBlockedForHost` instead of
    the bare `IsBlocked`, so both real enforcement points honor the
    narrower scoping — including at actual dial time, so a hostname that
    IS trusted but gets DNS-rebound to a private IP outside the
    IP-allowlist is still blocked.
  - **Fully backward compatible / opt-in:** when
    `SAFEHTTP_ALLOW_PRIVATE_HOSTS` is unset (true for every fleet service
    today — confirmed nothing currently sets `SAFEHTTP_ALLOW_PRIVATE_IPS`
    either), `IsBlockedForHost` behaves identically to `IsBlocked`. The
    plain `IsBlocked(ip)` function, its doc comment, and every existing
    call site that only ever sees a raw IP (no hostname to scope by,
    e.g. the egress-allowlist transport's IP-literal fast path) are
    unchanged.
  - New tests: `safehttp/allow_private_hosts_test.go` (unit-level
    `IsBlockedForHost` coverage, including the suffix-confusion and
    "untrusted hostname sharing the allowed IP" regression cases) and
    `safehttp/allow_private_hosts_internal_test.go` (real hostname
    resolution through `GuardHost`, plus dialer-level DNS-rebind
    adversarial cases mirroring the existing
    `dnsguardcache_rebind_test.go` methodology).

## v0.89.1 — 2026-08-23

### Security

- Bumped `golang.org/x/net` v0.43.0 → v0.55.0 in the root module (GitHub
  Dependabot alert #1, medium: Go's HTML parser is vulnerable to a denial
  of service).
- Bumped `google.golang.org/grpc` v1.80.0 → v1.82.1 and
  `golang.org/x/net` v0.52.0 → v0.55.0 in the `telemetry` nested module
  (Dependabot alert #2, high: gRPC-Go xDS RBAC and HTTP/2
  vulnerabilities). Transitive `golang.org/x/sys`/`golang.org/x/text`/
  `google.golang.org/genproto` bumps followed from `go mod tidy`.
  `go build`/`go vet`/`gofmt`/`go test` clean in both modules; no source
  changes, dependency versions only.

## v0.89.0 — 2026-08-23

### Added

- **New package `wordbound`** — the canonical fix for a Go regexp footgun
  that independently bit two fleet services: `\b` (word boundary) is
  ASCII-only, so it silently misbehaves next to any non-ASCII letter
  (Cyrillic, Han, Hangul, Greek, or an accented/extended Latin letter).
  Two confirmed real incidents motivated this: `go_gdpr_compliance`'s
  `legalFormSuffixRE` false-matched Latvian business-hours text
  ("S.-Sv. Slēgts") as a company name because `\b` incorrectly asserted a
  boundary right after "Sl" (the next character, "ē", isn't an ASCII word
  char); `go_update_frequency`'s `monthNameMDYRE` silently truncated
  Turkish "Şubat" (February) to "ubat" because a leading `\b` couldn't
  assert a boundary before "Ş". `go_legal_entity` had already solved this
  correctly years earlier with a manual rune-decoding `containsSuffix`/
  `isWordRune`/`prevRune` trio; `go_tos_finder` vendored a byte-for-byte
  copy of it rather than importing it (its own file comment already
  flagged this as tech debt), because no shared home existed until now.
  `wordbound` promotes that implementation to a single source of truth,
  exposed two ways: `ContainsToken`/`IsWordRune`/`PrevRune`/`IsGluedScript`
  for manual scanning, and the `LeadingBoundary`/`TrailingBoundary` regex
  fragment constants for splicing into hand-built patterns in place of a
  bare `\b`. `go_legal_entity`, `go_tos_finder`, and `go_update_frequency`
  are being migrated onto it in the same pass as this release.

## v0.87.0 — 2026-08-12

### Added

- **`safehttp.ErrDomainDenied` + `safehttp.IsDomainDenied` / `safehttp.DeniedDomains()`**
  — a hardcoded, fleet-wide compliance denylist (sci-hub.st, sci-hub.ru,
  thepiratebay.org, thepiratebay.se, rapidgator.net, avito.st, avito.ru,
  avitop.com, rutracker.org, ddos-guard.net, vglista.no). Our upstream
  proxy provider named these copyright-infringement-related targets and
  continuing to route requests to them risks the account being banned
  outright. Enforced inside `GuardHost`, which both `CheckURL` (the
  handler front-gate used by go-html-proxy/go-js-proxy/domainscope) and
  every `safehttp.NewClient` dialer already call — a blocked host never
  reaches DNS or the wire, and this one bump propagates the block to
  every consumer without per-service code changes. Matching is
  exact-or-subdomain and case/trailing-dot insensitive. Distinct error
  from `ErrBlocked` (private-network/SSRF) and `ErrEgressNotAllowed`
  (per-client allowlist) so callers can surface a specific compliance
  message instead of a generic block.

## v0.86.0 — 2026-08-12

### Fixed

- **`fleetfetch` fallback recursion on cache-side 400/401/403** — the
  built-in direct fallback is now constructed with
  `safehttp.WithoutFetchCache()`, and every fallback request carries
  `safehttp.WithoutFetchCacheContext`. Previously, the fallback's normal
  `safehttp.Client` resolved the process-wide fetch-cache delegate at call
  time and re-entered the same `fleetfetch.Client`: cache rejection →
  fallback → default delegate → cache rejection, repeating until a caller
  deadline and generating a request storm. The context guard also protects
  caller-supplied ordinary `safehttp` fallback clients.
- **Cache authentication errors are terminal and circuit-broken** — a
  cache-side 401/403 without fetch-cache provenance now returns typed
  `*fleetfetch.CacheAuthError` (`errors.Is(err,
  fleetfetch.ErrCacheAuth)`), opens a permanent circuit for that Client,
  and suppresses subsequent attempts with the same immutable API key.
  `safehttp`'s process-wide delegate still fails open to its own direct
  transport; direct `fleetfetch` consumers see the configuration error and
  must create a new Client after rotating credentials.

## v0.85.0 — 2026-08-09

### Added

- **`server.RunTier(serviceName, version, handler, requiredTier string, enforce bool, opts ...Option)`** —
  `Run` with the prepended auth swapped from `WithKeystoreAuth` to
  `WithKeystoreAuthTier` (v0.84.0). `Run` unconditionally prepends
  legacy local-token auth; a handful of pentest-tagged
  services (Coolify-runtime, 5-line `main.go` via `Run`, not `New`) need
  their direct entrypoint tier-gated too. Passing `WithKeystoreAuthTier`
  as an extra `Run` option would stack two keystore verifies (the
  prepended untiered one still admits any valid key or configured local credential,
  then the tier one runs second) — correct but wasteful and confusing.
  `RunTier` replaces the prepended option outright, same one-verify-per-
  request shape as `Run`.

## v0.84.0 — 2026-08-09

### Added

- **`server.WithKeystoreAuthTier(requiredTier string, enforce bool, localTokens ...string) Option`** —
  the direct-service-path counterpart to `go-fleet-mcp-gateway`'s tier
  enforcement. Closes the bypass where a caller who skips the gateway and
  hits a service's own `/mcp` endpoint directly was never subject to
  `access_tier` at all. Mirrors `WithKeystoreAuth`/`WithKeystoreAuthMesh`'s
  established pattern, wiring `middleware.KeystoreOpts.RequiredTier`/
  `.TierEnforce` (added in v0.82.0). `localTokens`
  are NOT exempt — they resolve to an empty caller tier by construction, so
  they fail closed against any non-empty `requiredTier` the same way a
  live keystore-verified caller with no tier would.

## v0.83.0 — 2026-08-08

### Added

- **New header constant `header.RequiredTier` (`X-Required-Tier`)** —
  correction to the `access_tier` design: enforcement was originally
  attempted via a post-hoc nginx `if ($auth_tier != ...) { return 403; }`
  after `auth_request`, which is broken by nginx's phase ordering — `if`
  runs in the REWRITE phase, entirely before `auth_request`'s ACCESS
  phase even fires, so the check always saw an empty variable regardless
  of textual placement (found live, locked out every caller of a real
  pilot service for a few minutes). The fix moves the comparison inside
  the `/verify` subrequest itself: nginx sends `X-Required-Tier` (from a
  service's `access_tier` registry override) alongside the existing
  `X-Expected-Scope`, and the keystore denies inside `/verify` — the
  same mechanism `X-Expected-Scope`/scope enforcement already uses
  correctly.

## v0.82.0 — 2026-08-08

### Added

- **`apikey.TierSatisfies(callerTier, requiredTier string) bool`** — the
  fleet's single, testable policy for "does this caller's tier satisfy
  this resource's tier requirement." Exact-string-equality, fail-closed
  (`requiredTier != "" && callerTier == ""` → false). Deliberately no
  hierarchy or multi-tier-per-key set matching for v1 — a caller needing
  two tiers holds two keys.
- **`middleware.KeystoreOpts.RequiredTier` / `.TierEnforce`** —
  `TokenAuthKeystore` now gates dispatch through `TierSatisfies` when
  `RequiredTier` is set, mirroring this fleet's own
  `LEDGER_OVERFLOW_ENABLED`/`ENFORCE` shadow-then-enforce pattern
  (ADR-0036/0037): `TierEnforce=false` observes what *would* be denied
  without breaking traffic; `TierEnforce=true` actually returns 403.
  **Every** trust path funnels through one shared check before dispatch —
  the local-token fast path and `TrustPrivateMesh` never verify a real
  tier, so they resolve to `callerTier == ""` and fail closed against any
  non-empty `RequiredTier` by construction, not by each call site
  remembering to special-case it. Regression tests prove both have no
  escape hatch. Default `RequiredTier == ""` — zero behavior change for
  every service that doesn't opt in.
- **New `AuthResult` values** `deny-tier` / `shadow-deny-tier` — fully
  generic in `promx.AuthCollectors` (no new metric needed), so a service
  gating on `RequiredTier` gets its shadow-mode would-be-denied rate on
  the existing `apikey_auth_total{result=...}` metric for free.

## v0.81.0 — 2026-08-08

### Added

- **`apikey` package gained a `Tier` field** on `IssueRequest`, `IssueResult`,
  `VerifyResult`, and `KeyMeta` — the first-class extension point for
  per-key access tiers (e.g. `"open"`, `"free"`, `"vetted-pentest"`),
  needed so the fleet's keystore can express caller trust level, not just
  identity (`user`) and hostname allow-list (`scope`). Fully additive: a
  key with no tier verifies exactly as before, with `Tier == ""`.
- **New header constant `header.AuthTier` (`X-Auth-Tier`)** — `/verify`'s
  success path has no JSON body (nginx `auth_request` only forwards
  headers), so the tier is carried as a response header, mirroring the
  existing `X-Auth-User`/`X-Auth-Scope` convention exactly.
- **`middleware.TokenAuthKeystore` now propagates `X-Auth-Tier`** onto the
  incoming request after a successful keystore verify, using the same
  `.Set` (not `.Add`) clobber-not-append pattern already used for
  `X-Auth-User`/`X-Auth-Scope` — a client-forged `X-Auth-Tier` header is
  always overwritten with the keystore's real answer, never trusted.
  Regression tests: `TestKeystore_KeystoreApproves_SetsAuthTierHeader`,
  `TestKeystore_KeystoreApproves_ClobbersClientSuppliedTierHeader`.

## v0.80.0 — 2026-08-07

### Fixed

- **SECURITY: `server/mcp.go`'s `copyRequestContext` was a denylist
  (`Authorization`/`X-API-Key`/`Content-Type`/`Content-Length` only),
  so any other header an MCP caller sent — including a privileged one
  like `X-Admin-Token` that has nothing to do with this bridge — rode
  along silently into the synthesized in-process replay and reached
  whatever handler happened to check for it. Flagged 2026-08-07 during
  a fleet-wide MCP security review. Replaced with an explicit allowlist
  (`Accept`, `Accept-Language`, `Cache-Control`, `User-Agent`,
  `X-Fleet-Caller`, `X-Forwarded-For`, `X-Forwarded-Host`,
  `X-Forwarded-Proto`, `X-Real-Ip`) confirmed against real handler usage
  across the fleet — an allowlist is safe by construction against any
  *future* privileged header too, not just the ones known dangerous
  today. New regression test `TestWithMCPDoesNotForwardUnlistedHeaders`
  locks this in.
  - summary: `server/mcp.go`'s MCP bridge copied any request header a caller sent into the replayed request; it now forwards only an explicit allowlist of headers.

## v0.79.0 — 2026-08-06

### Added

- **`ledger.Client` gained `Balance(ctx, cred)`**, a read-only
  `GET /v1/balance` call that returns the caller's current balance
  without spending anything (ADR-0033, ADR-0036 Phase 1). Previously the
  client only exposed `Charge`, which mutates state on every call —
  repurposing it as a probe ("charge 1 token to check access") would have
  meant every caller doing an allow/deny check pays for the privilege.
  `Balance` mirrors `Charge`'s error handling (`ErrNoCredential`,
  `ErrLedgerUnavailable`) and reuses the same envelope-decode pattern.

### Fixed

- **`ledger.New()`'s `LEDGER_SERVICE_URL` default pointed at the ledger's
  pre-collision-fix port (18208) instead of its current registry port
  (18314).** Anyone constructing a client without setting the env var
  explicitly silently talked to nothing. Fixed the default; callers that
  already set `LEDGER_SERVICE_URL` are unaffected.

## v0.78.0 — 2026-08-06

### Added

- **`apikey.IssueRequest`/`IssueResult`/`KeyMeta` gained an optional
  `Email` field.** Lets a caller tag a key with an owner's email address
  so keys issued separately (e.g. one per service) can be grouped as
  belonging to the same account. Purely advisory metadata, not used for
  auth — omitted from the wire when empty (`omitempty`).

## v0.77.0 — 2026-08-05

### Fixed

- **`server.WithMCP()`'s tools/call replay dropped every request-identity
  header, so any handler that inspects the caller saw an anonymous
  request.** `mcpToolHandler` synthesized a brand-new `*http.Request` via
  `http.NewRequestWithContext` for the in-process replay against `s.Mux`,
  carrying none of the original inbound request's headers or RemoteAddr.
  Reproduced live against `go-fleet-ip`: its `?json` response came back
  with `ip`, `forwarded_for`, and `user_agent` all empty when called
  through `/mcp`, even though the identical request over plain HTTP (or
  through `go-fleet-mcp-gateway`, which forwards `X-Forwarded-For`
  separately) worked correctly — the gap was specifically in the
  in-process bridge. New `copyRequestContext` propagates the MCP caller's
  headers (`X-Forwarded-For`, `X-Real-IP`, `User-Agent`, `Accept-Language`,
  etc., available via `CallToolRequest.Extra.Header`) onto the synthesized
  request before replay, skipping `Content-Type`/`Content-Length` (already
  correctly set for the synthesized body) and `Authorization`/`X-API-Key`
  (auth already ran against the inbound request; the replay doesn't need
  its own credentials — see this file's package doc). Any service using
  `WithMCP()` was affected, not just IP-echo services — this is the
  general fix, not a `go-fleet-ip`-specific one.

## v0.76.0 — 2026-08-05

### Added

- **New `ledger` package — the canonical client for
  `go-fleet-token-ledger` (ADR-0033).** `ledger.New().Charge(ctx,
  cred, amount, reason)` deducts tokens from a caller's prepaid
  balance, attributed via `Credential` — never the calling service's
  own `FLEET_API_KEY`. `ledger.CredentialFromRequest(r)` forwards the
  exact token `server.WithKeystoreAuth` already verified for the
  inbound request, the same caller-credential-forwarding model
  `go-fleet-mcp-gateway` uses (ADR-0032). A 402 response comes back as
  `*PaymentRequired` (`errors.As`), carrying the ledger's x402-shaped
  body verbatim so `ledger.WritePaymentRequired` can proxy it straight
  through to the metered service's own caller without this client
  re-deriving the shape.

  This is the "fleet rule: change the library, not every service" for
  metering — a service opts into charging a caller with three lines
  in its handler; the account/balance/402 logic lives here once,
  not duplicated per service.

### Changed

- **`middleware.extractToken` is now exported as `ExtractToken`.**
  Same three-shape lookup (`Authorization: Bearer` → `X-API-Key` →
  `?api_key=`), now reusable outside the middleware package —
  `ledger.CredentialFromRequest` is the first consumer. No behavior
  change for existing callers of `TokenAuthKeystore`.

## v0.75.0 — 2026-08-05

### Added

- **`agent.Tool.Path` now supports `{name}` placeholders, and a new
  `agent.Tool.BodyField` covers raw-body services.** Discovered
  onboarding the first real pilot services (Phase 5 of the MCP
  rollout, go-fleet-dig / go-fleet-md / go-fleet-pwgen):
  `server.WithMCP()`'s original two shapes — GET+query-string,
  POST+JSON-object-body — didn't cover services whose primary input
  is path-embedded (`go-fleet-dig`: `GET /{type}/{name}`) or a raw
  body with separate query knobs (`go-fleet-md`: `POST /` with the
  markdown itself as the body, `?to=`/`?width=` alongside it).

  `Path: "/{type}/{name}"` substitutes matching `InputSchema`
  properties before the request is built; whatever's left becomes
  query parameters (GET/HEAD/DELETE) or the JSON body (default,
  non-GET). Setting `BodyField: "markdown"` sends
  `args["markdown"]` as the raw, un-encoded request body instead —
  every other property still becomes a query parameter.
  `BodyContentType` overrides the default `text/plain; charset=utf-8`.

  Both fields are additive; existing `agent.json` files and the
  original two shapes are unaffected.

## v0.74.0 — 2026-08-05

### Added

- **`server.WithMCP()` — bridge a service's agent contract onto a real Model
  Context Protocol Streamable HTTP endpoint at `/mcp`.** Phase 1 of making
  fleet services directly usable as MCP tools from Claude/Codex/other
  agents, using the official `github.com/modelcontextprotocol/go-sdk`
  (pinned `v1.5.0`, stable Streamable HTTP + OAuth support).

  Each `agent.Tool` in the service's `agent.Contract` (see `WithAgent` /
  `WithAgentFromEmbed`, `v0.73.0`) becomes one MCP tool. A `tools/call`
  replays as an in-process `*http.Request` against the service's own
  `s.Mux` — the same route its HTTP callers hit — so the MCP surface can
  never drift from the real handler. No second auth path either: `/mcp`
  sits behind the same middleware chain (`WithKeystoreAuth`) as every
  other route, so it takes the same `Authorization: Bearer <key>` the
  fleet keystore already issues.

  `WithAgent`/`WithAgentFromEmbed` and `WithMCP` can be passed to
  `server.New` in either order — the contract is resolved into
  `Server.AgentContract` during option application, and `/mcp` is
  mounted after the full option list has run.

  New optional `agent.Tool` fields `Method` and `Path` tell the bridge
  how to build the synthetic request (default `GET "/"`, matching the
  existing `DefaultTool` convention of a query-string-driven primary
  input). Additive — existing `agent.json` files with no `method`/`path`
  keep working unchanged.

  ```go
  srv := server.New(cfg,
      server.WithAgent(agent.DefaultContract(cfg.AppName, cfg.Version)),
      server.WithMCP(),
  )
  ```

  `WithMCP()` with no agent contract panics at `New()` — a service with
  nothing to expose as a tool has no reason to mount `/mcp`.

## v0.73.1 — 2026-08-04

### Fixed
- **`fleetfetch`: coordinated renderer overload without multiplying work.**
  Canonical `load_shed` responses for JS/HTML/network render requests now
  return a typed `ErrRenderBusy` / `*RenderBusyError`, preserving the exact
  `Retry-After` value instead of launching one unshared direct request per
  caller and silently substituting raw HTML for rendered evidence. Default
  static requests retain their semantically equivalent direct fallback.
  Origin/negative-cached 5xx responses carrying `X-FetchCache-Fetched-At`
  are now replayed as-is instead of bypassing the negative cache. Genuine
  cache transport failures and untyped cache-service 5xx responses retain
  the previous direct-fallback behavior. The internal one-hop loop marker
  now travels as cache control metadata rather than leaking to origins and
  fragmenting representation cache keys. Added exhaustive direct/render,
  cache-replay, outage, Retry-After, and HTTP-adapter regression coverage.

## v0.73.0 — 2026-08-01

### Fixed
- **`dataformat`: encoding to XML no longer trusts a decoded key as a
  well-formed element/attribute name** — found during a TRL audit of
  `go_data_format_converter`. `writeXMLElement` wrote a `map[string]any`
  key (and, for `-`-prefixed attribute keys, the attribute name) straight
  into the output buffer via `xmlEscapeName`, which is a no-op writer —
  correct for the *decode* path (names there came from `xml.Unmarshal`
  parsing already-well-formed markup) but not for *encode*, where the key
  is an arbitrary JSON/YAML/TOML/CSV field name with no such guarantee.
  Two concrete, evidence-backed failure modes: (1) an ordinary field like
  `"first name"` produced syntactically invalid XML
  (`<first name>Ada</first name>`) instead of erroring; (2) a key
  containing markup metacharacters was an outright **XML injection** —
  `{"root":{"a</root><evil>injected":"x"}}` converted to
  `<root><a</root><evil>injected>x</a</root><evil>injected></root>`,
  closing the enclosing tag and splicing in attacker-controlled sibling
  markup rather than rendering the key as inert text. Both were silent:
  no error was returned in either case, so `go_data_format_converter`'s
  handler reported `HTTP 200` with a `result` field containing broken or
  injected markup instead of the existing non-fatal
  `degraded:["convert_failed:…"]` path it uses for every other
  unrepresentable shape. Fixed by adding `isValidXMLName` (an ASCII XML
  1.0 Name-production check, plus the reserved `xml*` prefix) and having
  `writeXMLElement` validate both the element name and every attribute
  name *before* writing anything, returning `ErrUnsupportedShape` (the
  package's existing "target format can't represent this" convention) on
  failure instead of ever emitting unescaped/invalid markup. Ordinary
  identifier-shaped keys (letters, digits, `_`, `-`) are unaffected — see
  `TestXMLEncodeAcceptsOrdinaryIdentifierKeys`; the injection/malformed
  cases are covered by `TestXMLEncodeRejectsInvalidNames`
  (`dataformat/dataformat_test.go`). Not a wire-format change: only
  previously-silent bad output is affected, and it now surfaces through
  the same degrade path callers already handle.

## v0.72.0 — 2026-07-12

### Fixed
- **`safehttp`: `WithForceHTTP2()` now bypasses the default fetch-cache
  delegate** — BREAKING (behavior, not signature) for any client that
  combines `WithForceHTTP2()` with the process-wide fetch-cache (any
  service with `FLEET_FETCH_CACHE_URL` set, which `server.New` auto-wires
  via `SetDefaultFetchDelegate`). Root cause of the 2026-07 round-12
  follow-up: `go_page_load_metrics` added `WithForceHTTP2()` to both its
  client-construction call sites and confirmed the option compiled into
  the running binary, yet `https://page-load-metrics.0crawl.com/?target=…`
  kept reporting `protocol:"HTTP/1.1"` against known-HTTP/2 origins
  (verified cloudflare.com). The fix was correct — `newBaseTransport`
  wires `ForceAttemptHTTP2` exactly as documented, confirmed by
  `TestWithForceHTTP2_NegotiatesH2OnHEAD` — but it had zero effect in
  production because eligible GETs never reached that transport at all:
  `useDefaultFetchCache` was `true` for any non-`WithoutProxy` client
  (the required posture for `FLEET_REQUIRE_PROXY=1` / `proxy_egress:
  true` services), so `extrasTransport.RoundTrip`
  (`extras_extras_transport.go`) routed the probe through the fleet
  fetch-cache delegate instead. That path synthesizes its `*http.Response`
  from cached bytes with **no live TLS connection behind it** — `resp.TLS`
  is always `nil` and `resp.Proto` is a hardcoded `"HTTP/1.1"` — because
  `FetchResult`/`fleetfetch.Response` carry no protocol/ALPN field at all
  on the current wire contract. `NegotiatedProtocol(resp)` therefore always
  returned `""` for a delegate-served response regardless of what the
  origin actually negotiated, silently defeating the option. Confirmed via
  a minimal repro (`safehttp.NewClient(WithForceHTTP2())` against
  cloudflare.com): direct egress and proxy-tunneled egress (a local
  CONNECT proxy, standing in for the production Webshare proxy) both
  correctly negotiate real h2 (`resp.Proto="HTTP/2.0"`,
  `NegotiatedProtocol="h2"`) — ruling out the dialer/TLSClientConfig/proxy
  hypotheses entirely — while installing ANY `DefaultFetchDelegate` flips
  the same client to `resp.Proto="HTTP/1.1"`, `resp.TLS=nil`,
  `NegotiatedProtocol=""`, independent of the real origin's capability.
  `WithForceHTTP2()` now sets `useDefaultFetchCache=false` (mirrors
  `WithoutProxy`/`WithoutFetchCache`), so a caller who explicitly asked to
  observe the real negotiated ALPN protocol always gets a live connection.
  An explicit per-client `WithFetchDelegate(...)` is a deliberate opt-in
  and still wins over this. New regression tests:
  `TestDefaultFetchDelegate_WithForceHTTP2IgnoresDefault` and
  `TestWithFetchDelegate_PerClientWinsOverForceHTTP2`
  (`safehttp/fetchcache_test.go`) — both fail against the pre-fix code
  (delegate served a 200 instead of the real origin's 203) and pass with
  the fix.
- Documented the same caveat on `NegotiatedProtocol`, `WithForceHTTP2`, and
  `WithoutFetchCache` doc comments: an empty/`"HTTP/1.1"` reading from a
  fetch-cache-routed response means "unknown", not "confirmed HTTP/1.1" —
  the current `FetchResult`/`fleetfetch` wire contract has no channel for
  the fetch-cache's own upstream protocol at all. Adding one (an
  `X-FetchCache-Upstream-Protocol` response header from the fetch-cache
  service through to a `FetchResult.Proto` field) is a separate,
  cross-repo follow-up — the fetch-cache service itself lives outside this
  repo.

## v0.71.0 — 2026-07-05

### Added
- **`nullify` package**: `StringOrNil` / `IsPlaceholder` — the canonical
  "checked and found nothing" helper for enrichers writing a
  verdict/classification column. A 2026-07-05 live audit of the
  `domain_*` enrichment tables
  (go-url-categorizer-api `docs/audit/2026-07-05-enricher-quality-audit.md`)
  found at least six enrichers writing a literal placeholder string
  (`"unknown"`, `"n/a"`, `""`) instead of `NULL` when no real signal
  was found — the same missing call reintroduced six times, not six
  unrelated bugs. New enrichers should call `nullify.StringOrNil(...)`
  at the point a classification is about to be persisted; existing
  offenders should adopt it per-repo (tracked outside this repo).

## v0.70.0 — 2026-06-30

### Fixed
- `server.New()` `/health` handler now returns `"status":"ok"` (was `"healthy"`); fleet E01 eval and fleet-runner smoke gate step 8 now pass for all services using this scaffold
- `server.New()` `/version` handler now returns `application/json {"version":"<ver>"}` (was `text/plain` bare string); fixes fleet-runner version-match check on deploy

## v0.69.0 — 2026-06-29

### Added

- **`fleetfetch` now sends `X-Fleet-Caller` on every cache request**, so the
  fetch cache can forward the calling service's identity to `go-js-proxy` /
  `go-html-proxy` and those renderers can attribute load per-enricher (their
  `/stats` + `/metrics` were previously seeing the cache as one anonymous
  hop). Resolution order: per-client `WithCaller(id)` → process-wide
  `fleetfetch.SetDefaultCaller(id)` → env (`FLEET_SERVICE_ID`, `SERVICE_ID`).
  When none resolves, no header is sent — un-migrated callers are unaffected.
- **`server.New` seeds the default caller from `cfg.AppName`** automatically,
  so every service on the standard server (and the safehttp→fetch-cache
  delegate it wires) propagates its identity with **zero per-service code
  change** — a `go-common` bump + redeploy is all that's needed. The header is
  set only on the internal cache request, never on the direct-to-origin
  fallback (no internal-header leak to public sites).

## v0.65.0 — 2026-06-08

### Added

- **`loadshed` — a non-blocking concurrency gate for bounding in-flight
  calls to a slow, saturatable shared upstream.** Generalises the
  hand-rolled `renderSem` semaphore landed in
  `go_infrastructure_fetch_cache` 0.3.8 (PR #12), where a backfill
  fan-out piled ~8.3k goroutines on a saturated `go-js-proxy` (host
  loadavg 56/20), thrashing the scheduler until downstream enrichers'
  `/selftest` probes stalled past their deadline and rolled back healthy
  fleet deploys. The pattern — "bound concurrent expensive calls to a
  slow sibling, fast-503 the excess instead of piling goroutines" —
  recurs across every service that proxies to a saturatable renderer
  (go-js-proxy / go-html-proxy / …).

  `loadshed.New(name, limit)` returns a `*Gate`; `gate.TryAcquire()` is
  a non-blocking semaphore acquire returning an idempotent `release`
  func and an `ok` bool. On `ok == false` the caller fails fast via
  `loadshed.WriteShed(w, retryAfter, msg)`, which writes the canonical
  503 + `Retry-After` + `{error_code:"load_shed"}` envelope. For the
  "gate a whole endpoint" case, `gate.Guard(retryAfter, msg)` is
  net/http middleware. `limit <= 0` leaves the gate unbounded (the
  test/opt-out default).

- **`promx.NewLoadshedCollectors` + `promx.AutoLoadshed`** — `AutoWire`
  now installs a process-wide `loadshed.Observer`, so every gate emits
  `loadshed_shed_total`, `loadshed_admitted_total`, and
  `loadshed_in_flight` (labelled `{service, gate}`) with zero per-gate
  wiring. `loadshed_shed_total` is the canonical "this service is
  shedding load" alert signal.

  Migrate a hand-rolled `chan struct{}` shed semaphore by swapping the
  field for a `*loadshed.Gate`, replacing the inline
  `select { case sem <- …: default: 503 }` with
  `TryAcquire()`/`WriteShed`, and dropping any bespoke `*_shed` counter
  (the metric is now emitted fleet-wide via promx). `go_infrastructure_
  fetch_cache` is the first consumer.

## v0.64.0 — 2026-06-08

### Added

- **New `meshresult` package — fleet-wide enricher result classification.**
  Collapses the per-repo `httpStatusFor` boilerplate that ~70 enrichers
  hand-rolled (classify a fetch/DNS error → ok/no_data/unreachable/error,
  map to an HTTP code domainscope keys on, emit a machine-readable body
  field) into one tested place. Imports only `response` + stdlib — no
  safehttp/fleetfetch cycle.

  - `type Outcome string` with `OutcomeOK` / `OutcomeNoData` /
    `OutcomeUnreachable` / `OutcomeTimeout` / `OutcomeError`.
  - `func (Outcome) HTTPCode() int` — the contract domainscope keys on:
    `ok→200`, `no_data→404`, `unreachable→404`, `timeout→504`,
    `error→502` (unknown→502, conservative). 404 for both no_data and
    unreachable is intentional (both become NoData(4) in domainscope
    today); the body `result` field preserves the finer distinction.
  - `func ClassifyFetchError(err error) (Outcome, string)` — maps a
    fetch/DNS/transport error to an Outcome + a stable machine-readable
    reason token (`dns_nxdomain`, `connect_refused`, `connect_timeout`,
    `tls_error`, `fetch_timeout`, `upstream_4xx`, `upstream_5xx`,
    `decode_error`, `bad_request`, …) via stdlib typed checks
    (`context.DeadlineExceeded`, `*net.DNSError`, `net.Error.Timeout()`)
    plus documented string markers for the upstream/fleetfetch shapes.
    SSRF-block / bad-scheme are CALLER errors → `(OutcomeError,
    "bad_request")`, never laundered into a 404 NoData.
  - `func WriteOutcome(w, outcome, reason, data)` and `func
    OutcomeEnvelope(outcome, reason, data) (code, body)` — emit/build the
    `response` envelope with the right HTTP code AND stable top-level
    `result` + `reason` fields so domainscope can read the finer
    classification from the body in a future upgrade.

## v0.63.0 — 2026-06-08

### Changed

- **BREAKING (behavioural): `obs` is now opt-in, no longer auto-started by
  `server.New`.** v0.61.0 auto-wired the `obs` localhost debug server
  (`net/http/pprof` + a `/metrics` mirror) into `server.New`, which pulled
  `net/http/pprof` into the binary and started a debug listener
  (`127.0.0.1:6060`) in **every** service on the next go-common bump,
  whether it needed pprof or not. That coupling is removed: `server.New` no
  longer imports or starts `obs`. A service that wants pprof + the localhost
  debug/metrics server now opts in explicitly with one line in `main()`:

  ```go
  stop, _ := obs.Init()   // reads DEBUG_ADDR (default 127.0.0.1:6060); no-op if disabled
  defer stop()
  ```

  Services that do **not** import `obs` no longer compile `net/http/pprof`
  in or open a debug listener. The `obs` package itself is unchanged and
  fully standalone (loopback-only default, `DEBUG_ADDR=off`/`OBS_DISABLE=1`
  to disable, bind error non-fatal); `RegisterRuntimeMetrics` is unchanged
  for services with their own registry. The default `/metrics` runtime
  metrics for `server`-based services are unaffected — those come from
  `promx` and stay as-is. Supersedes the auto-wire added in #34 / v0.61.0.
  - summary: `obs` is opt-in: `server.New` no longer starts the localhost pprof and `/metrics` debug server (127.0.0.1:6060); a service that wants it calls `obs.Init()` in `main()`.
  - off-switch: `stop, _ := obs.Init()` and `defer stop()` in `main()` bring the old debug server back

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
