# go-common

Shared Go primitives for the fleet's services: the HTTP server bootstrap, the
SSRF-safe client, typed config and errors, the API-key client, and the small
coordination clients (rate limits, backoff, locks of a sort) that every service
would otherwise write for itself.

```go
import "github.com/baditaflorin/go-common/safehttp"
```

Module `github.com/baditaflorin/go-common`, Go 1.25. This repository is public:
nothing sensitive belongs in it.

**Change go-common, not the consumers.** When a fix would touch every service,
fix it here and bump the dependency. That only works if releases are
trustworthy, which is what the API policy and the CI gates below are for.

## Which package do I use?

| I need to... | Use |
| --- | --- |
| Start an HTTP service with the fleet middleware, `/health`, `/selftest`, graceful drain | [`server`](server/) with [`config`](config/) and [`env`](env/) |
| Call the **public internet** | [`safehttp.NewClient`](safehttp/) (refuses internal addresses, DNS-rebind safe) |
| Call a **sibling fleet service** on the private mesh | [`meshclient`](meshclient/): you must choose fail-open or fail-closed, and in fail-open it adds `"<primitive>-down"` to the request's [`degraded`](degraded/) list. `safehttp` rejects private IPs by design, so it cannot be used here |
| Fetch a page through the fleet fetch cache | [`fleetfetch`](fleetfetch/) |
| Send a browser through an upstream proxy that needs credentials | [`egressproxy`](egressproxy/) with [`proxysupplier`](proxysupplier/) |
| Verify an API key, or protect a handler | [`apikey`](apikey/), [`middleware`](middleware/) |
| Read environment variables | [`env`](env/); declare the ones your package owns in its registry so they are listed in [docs/ENV.md](docs/ENV.md), checked at start-up and shown redacted on the debug server |
| Write structured logs with the service, version and trace id | [`logx`](logx/): set `LOG_FORMAT=json` (or pass `server.WithStructuredLogs()`); off by default |
| Return JSON, typed errors, pages | [`response`](response/), [`errors`](errors/), [`pagination`](pagination/) |
| Decode and validate a request body | [`validate`](validate/) |
| Retry, or protect a slow upstream | [`resilience`](resilience/) (retries limited by a budget, plus a breaker and a load-shed gate), built from [`retry`](retry/), [`circuitbreaker`](circuitbreaker/) and [`loadshed`](loadshed/); also [`backoffcoord`](backoffcoord/), [`ratecoord`](ratecoord/) |
| Decide which result type to return | [docs/outcomes.md](docs/outcomes.md): `errors`, `meshresult`, `degraded`, `nullify` and what each is for |
| Decide which observability package to use | [docs/observability.md](docs/observability.md): what `promx`, `metrics`, `reqstats`, `logx`, `telemetry`, `graph` and `obs` each emit, what `server.New` wires by default, and what each costs per request |
| Fake time or an upstream in a test | [`clock`](clock/) (`testhelpers/testclock`), [`testhelpers/httpmock`](testhelpers/httpmock/) |
| Test a URL or host guard against known SSRF bypasses, or fail a test that leaks goroutines | [`testhelpers/ssrfcorpus`](testhelpers/ssrfcorpus/), [`testhelpers/leakcheck`](testhelpers/leakcheck/) |

`client.Client`, `client.New` and `client.Post` are deprecated in favour of `safehttp.NewClient` or `meshclient`; the `client.Fetch` and `JSProxy*` helpers have no replacement and
are not deprecated. `shutdown` is deprecated in favour of `server`'s graceful drain. How many repositories import each package: [docs/package-usage.md](docs/package-usage.md).
What the small packages may import: [docs/dependencies.md](docs/dependencies.md).

Runnable examples (checked by `go test`) are in `example_test.go` next to the
packages above. The full list of packages is in [docs/PACKAGES.md](docs/PACKAGES.md),
generated from the packages' own doc comments.

## Using go-common in a service

- **Pin a recent version** and bump it when a release lands; the changelog says
  what changed and whether it can break you ([CHANGELOG.md](CHANGELOG.md)).
- **Lint with the shared config** so deprecated API and the common HTTP mistakes
  show up in your CI: see [lint/README.md](lint/README.md).
- **What a release promises** (and what it does not): [docs/api-policy.md](docs/api-policy.md).
- **Check that your pin has go-common's security fixes**:
  `go run github.com/baditaflorin/go-common/scripts/compatcheck@latest -gomod go.mod`
  (the floor and the fixes behind it are in [compat/compat.json](compat/compat.json);
  see [docs/releasing.md](docs/releasing.md) for backports to old lines).

## Working on go-common

Every change to the module has to pass, in CI (Woodpecker, `.woodpecker.yml`):

| Gate | What it catches |
| --- | --- |
| `gofmt`, `go vet`, `go build`, `go test` | The basics |
| `go test -race` | Data races |
| `golangci-lint` (new findings only) | Bugs and dead code; legacy findings do not block |
| `scripts/vuln-check.sh` (`govulncheck`) | A reachable known vulnerability that is not in the reviewed, expiring allowlist `scripts/govulncheck-allow.txt` |
| `scripts/forward-compat.sh` | A behaviour change Go makes for a newer `go` line (GODEBUG defaults): vet and the tests are run on a copy with the line raised, see [docs/releasing.md](docs/releasing.md) |
| `scripts/apidiff-check.sh` | An incompatible API change that the changelog does not call BREAKING |
| `scripts/bench-gate.sh` | A hot-path benchmark that started allocating more (allocs/op, against the merge base) |
| `scripts/release-check.sh` | A CHANGELOG version that is malformed, already tagged, or not the next free one |
| `scripts/fuzz.sh` (short smoke) | Panics and hangs on hostile input in the parsers |
| `go run ./scripts/pkgindex -check` | A stale package index, or a package with no doc comment |
| `go run ./scripts/envdoc -check` | A stale `docs/ENV.md`, or a package that declares environment variables but is not imported by the generator |
| `lint/selftest.sh` | The shared lint config no longer firing |

Most of these are CPU-heavy. Run them in CI or on a build host rather than a
laptop. `scripts/fuzz.sh 2m` is the long version of the fuzz smoke, meant for a
nightly job; a crashing input it finds must be fixed and then committed under
`testdata/fuzz/` so plain `go test` replays it forever.

Every release needs a `CHANGELOG.md` entry. A change that removes or alters
exported API, or changes a default or an environment variable, leads that entry
with **BREAKING:**; see [docs/api-policy.md](docs/api-policy.md). A new package
needs a package comment, or the index check fails.
