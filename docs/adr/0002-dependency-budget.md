# ADR-0002 (go-common) — A dependency budget for the small packages; no nested modules yet

* **Status**: Proposed
* **Date**: 2026-10-17
* **Authors**: claude-sonnet-5-5 (with florin)
* **Tags**: go-common, dependencies, go.mod, go.sum, scanners, nested modules
* **Scope**: what a consumer inherits from go-common's dependencies, a CI gate that keeps the small packages small, and a recommendation on splitting go-common into nested modules. No production code changes.

## Context

go-common is one Go module that requires pgx, otelsql, the MCP SDK, pyroscope, go-spiffe
(which brings gRPC; an advisory in gRPC has already forced a release, 0.102.2), go-qrcode,
securesystemslib, the OpenTelemetry SDK and more. Go compiles only the packages a consumer
imports, so build cost is not the question. The question is what a service that imports
only a small package, `env` or `header`, inherits in its `go.mod`, in its `go.sum` and in
every scanner alert, and whether anything stops a small package from growing a third-party
import tomorrow.

## What a consumer inherits (measured)

A throwaway module that imports one go-common package and runs `go mod tidy`, with go-common
taken from this tree through a `replace` directive (the `env` and `safehttp` rows were
repeated against the published 0.102.68 and match, apart from `go.sum` having 2 and 61
lines there):

| Imports only | `// indirect` requirements | `go.sum` lines | Modules in `go list -m all` |
| --- | --- | --- | --- |
| `env` | 0 | 0 | 56 |
| `qrterm` | 1 | 2 | 56 |
| `middleware` | 2 | 10 | 56 |
| `profiling` | 3 | 16 | 63 |
| `dbx` | 14 | 50 | 69 |
| `spiffe` | 9 | 48 | 83 |
| `safehttp` | 22 | 59 | 117 |
| `server` | 40 | 112 | 128 |

Two things follow.

1. go-common's own requirement list does **not** leak into a consumer's `go.mod` or
   `go.sum`. go-common is a Go 1.17+ module, so the module graph is pruned and `go mod tidy`
   keeps only the modules that provide packages the consumer builds. An `env`-only
   consumer has no third-party requirement at all. Only tools that read the module graph
   rather than the packages (`go list -m all`, some SBOM generators) see go-common's whole
   list, 56 modules even for `env`.
2. What a consumer inherits is exactly the set of third-party modules its imports reach,
   transitively through other go-common packages. The indirect-requirement counts above are
   the counts in [`docs/dependencies.md`](../dependencies.md) (`spiffe` shows 9 against 8:
   `tidy` also lists a module only Windows builds need). That page is generated, so this is
   the number that has to be watched, and it is large for packages that look small: `safehttp`
   reaches 22 modules because it imports `telemetry` and `graph`.

## Measurements (snapshot of 2026-10-17; the generated page is the live version)

65 importable packages; 38 reach no third-party module. The heavy ones:

* `server` 40; `chromeguard`, `egressproxy`, `obs`, `promx` 31; `client` 24;
  `fleetfetch`, `jsbundle`, `meshclient`, `pentest`, `safehttp`, `selftest`, `telemetry` 22;
  `dbx` 14; `deploymentlease` 10; `credentiallease`, `spiffe` 8; everything else 3 or fewer.
* The weight is `telemetry`: the OpenTelemetry SDK, the OTLP/HTTP exporter and, through the
  exporter's protobuf package, gRPC. 13 packages reach it (through `safehttp` for most) and
  16 reach gRPC (the 13, plus the three that use go-spiffe).
* `safehttp` imports `telemetry` on purpose: 0.102.0 wired fleet-wide tracing into
  `safehttp.NewClient` and `server.New`. The 22 modules are the price of that feature, not an
  accident.

Local consumers, counted from the clones under the shared projects directory: 19 distinct
consumer modules after collapsing 47 copies (dated worktrees and variants share a module
path). That is a lower bound: the fleet has about 110 services and not all are cloned.

| Third-party module reached through | Consumers (of 19) whose `go.mod` lists it |
| --- | --- |
| gRPC and protobuf | 19 |
| the OpenTelemetry SDK and OTLP/HTTP exporter | 18 |
| Prometheus client | 14 |
| pyroscope (through `server`), the MCP SDK (through `server`) | 11 |
| go-spiffe | 2 |
| securesystemslib | 1 |
| pgx, go-qrcode, toml | 0 |

Direct imports of the heavy packages among the 19: `server` 11, `safehttp` 9, `selftest` 8,
`telemetry` 6, `graph` 6, `promx` 4, `fleetfetch` 3, `client` 1; `dbx`, `spiffe`,
`profiling`, `qrterm` 0 (go-spiffe arrives through `credentiallease` and `deploymentlease`).
Module sets per consumer: one has 8, four have 22, three 31, ten 40, one 44. go-common is
pinned at 15 different versions by the newest copy of each of the 19 (18 across all copies).

## Decision

1. **Budget the small packages.** `scripts/dep-budget.txt` allows each budgeted package the
   third-party modules it reaches today and no more; `scripts/dep-budget.sh` (in the
   `validate` step) fails when one appears, naming the package, the module and the go-common
   package that imports it. It is a ratchet: today's sets are the starting point, including
   22 for `safehttp` and `selftest`, which this change does not claim is right. The page
   `docs/dependencies.md` is generated from the same measurement and checked in CI.
2. **Do not split go-common into nested modules now.**
3. **Name the lever that would matter** (not done here, it changes production code):
   `safehttp` reaches most of its 22 modules only through the OTLP exporter and the SDK.
   Keeping the trace API in a package that `safehttp` imports and moving the exporter and
   SDK wiring behind the entry point that `server` calls would leave `safehttp`, and the
   packages that reach the exporter through it, with the handful of modules `graph` and the
   trace API need instead of 22 (an estimate, not measured). It is an API question
   (`telemetry.Init` and friends move, or become wrappers that keep the heavy import), so it
   needs its own change and the API compatibility gate.

## Why not nested modules for `spiffe`, `profiling`, `dbx`, `qrterm`

What each would take out of go-common's module graph, by itself:

| Candidate | Modules only it (and its dependents) reach | Consumers it would help (of 19) | Blocker |
| --- | --- | --- | --- |
| `qrterm` | 1 (go-qrcode) | 0 | none, and nothing to gain |
| `dbx` | 5 (pgx, pgpassfile, pgservicefile, puddle, otelsql) | 0 | none, and nobody imports it here |
| `spiffe` | 2 (go-spiffe, go-jose); gRPC stays, `telemetry` reaches it | 2 | `credentiallease` and `deploymentlease` import it, so they would have to move too |
| `profiling` | 3 (pyroscope, godeltaprof, klauspost/compress) | 11, through `server` | `server` imports `profiling`: the split breaks `server` or makes the root module and the nested one require each other |

* The benefit is smaller than it looks. A consumer that does not import `dbx` has no pgx in
  its `go.mod` today (the table above), so splitting `dbx` changes nothing for it; it only
  shortens go-common's own graph, which matters to graph-level tools and to alerts raised
  against go-common's own `go.mod`. The one big, shared cost, gRPC and the OpenTelemetry
  stack in 18 or 19 of 19 consumers, is not touched by any of the four.
* The cost is real. The 19 local consumers pin 15 different go-common versions (18 across
  all copies, and the fleet is larger than the clones). Each nested module is another
  version line: its own tags, its own requirement on a go-common version, its own `go.sum`,
  and each root gate (API compatibility, release number, vulnerability allowlist, lint)
  would have to learn about it.
* The repository has run this experiment once. `telemetry` was carved into a nested module
  in 0.62.0 because nobody imported it (zero of about 438 consumers), and
  `scripts/build-modules.sh` had to be added because the root gate did not build it. It was
  folded back into the root module in 0.102.0, when `safehttp` and `server` started to use
  it. A nested module works only while nothing in the root imports it: the first time a root
  package needs it, it has to come back.

## Consequences

* A pull request that adds a third-party import to a budgeted package fails `validate`
  until the code moves or the budget line is edited with a reason. A pull request that adds,
  removes or re-imports a package also has to run `scripts/dep-budget.sh --write-doc`, as it
  already has to for `docs/PACKAGES.md` and `docs/ENV.md`.
* The step takes under a second in the CI image with a warm module cache (one `go list -deps`
  over the module per run of the script, plus a selftest that uses a fake `go`).
* The budget measures linux/amd64 without cgo, non-test imports and module paths only, so a
  dependency bump never changes it, and test-only imports are not covered.

## Revisit when

* A scanner or a policy starts to block consumers because of modules in go-common's graph
  that they do not build. The measurements above say that does not happen today; this is the
  one thing that would make splitting pay for consumers that never import the package.
* A new optional dependency is proposed that is heavy and that nothing in the root would
  import (a database driver family, a codec set). Make that a nested module from the start:
  it costs least before anything depends on it.
* The `telemetry` split above is done. The sets of `safehttp` and its dependents then drop,
  and the budget lines should follow them down.
