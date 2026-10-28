# What a service pays for importing go-common

Measured on 2026-10-28 against go-common 0.111.0, so that "go-common makes every service heavy"
can be argued with numbers. Go 1.26.10, linux/amd64, `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w'`
(the fleet's Dockerfile flags), one 4-core VM, containers capped at 3 cores and 2.1 GiB, medians of
12 runs with the range in brackets. Idle RSS is read from `/proc/PID/status` 10 s after the first
`GET /health` answered.

## The three shapes a fleet repository has

| | plain `net/http` hello | a `server.New` service | a CLI with `clock`, `version`, `logx` |
| --- | --- | --- | --- |
| Binary | 5.8 MB | 20.4 MB | 2.5 MB |
| Modules linked (`go version -m`) | 0 | 43 | 4 |
| Idle RSS | 6.0 MB | 19.6 MB | 3.2 MB |
| ...of it anonymous (private heap, stacks) | 1.0 MB | 2.6 MB | 0.8 MB |
| ...of it file-backed (the binary's pages) | 5.0 MB | 16.6 MB | 2.4 MB |
| Start to the first `200 /health` | 4.9 ms [3.4-10.6] | 14.1 ms [9.5-28.3] | n/a |
| Package initialisation (`GODEBUG=inittrace=1`) | 0.5 ms, 273 allocs | 4.1 ms, 5,794 allocs, 773 KB | n/a |
| Goroutines idle / threads | 2 / 5 | 6 / 8 | n/a |
| Per request, in process (`httptest`) | 0.6 us, 6 allocs, 274 B | 16.5 us, 77 allocs, 5.9 KB | n/a |
| Per request, over loopback | 92 us | 155 us | n/a |

Three things follow.

- **go-common's own code is small.** Its packages are 0.4 MB of the symbols in the service binary. The weight is
  what the service kit links: the OpenTelemetry SDK and OTLP exporter (with gRPC and protobuf), the Prometheus
  client, go-spiffe, the Pyroscope client and the MCP SDK. go-common has no `init()` function, and the 81
  package-level initialisers it does have (regular expressions, `env.Declare`, one `slog` logger) take under 0.3 ms
  together. The 4.1 ms of initialisation is a long tail of 217 packages, nearly all third-party: in three traced
  runs the largest single ones were the Prometheus client (0.4 to 0.7 ms, once 4.4 ms on a cold start), Pyroscope
  (1.7 ms in one run), `procfs` and gRPC (0.2 ms each).
- **Most of the idle RSS is not private memory.** 16.6 of the 19.6 MB are the executable's own pages, mapped
  clean from the page cache: shared by every replica of the same image and given back first under memory pressure.
  What a `server.New` service holds privately is 2.6 MB, 1.6 MB more than a plain `net/http` one.
- **The per-request cost is the one that is paid on every request.** 77 allocations and 16.5 us in process,
  63 us more than a bare handler over loopback. Half of the allocations are in the access-log middleware
  (`middleware.Logging`), and 13% of the CPU is a `slog.Logger.With` per request even when the path is skipped.

## Which feature costs what

Each row is a scratch build of the `server.New` service with one feature stubbed out; the last row stubs four.
A binary that drops only one of the two users of gRPC (the OTLP exporter, go-spiffe) keeps gRPC, so the rows
do not add up.

| Feature (what links it) | Binary | Idle RSS | Start to ready |
| --- | --- | --- | --- |
| everything (`server.New`) | 20.4 MB | 19.6 MB | 14.2 ms |
| without the Pyroscope client (`profiling`) | -0.5 MB | -0.8 MB | |
| without go-spiffe (`server` -> `spiffe`, for graph's mTLS) | -0.7 MB | -0.7 MB | |
| without the MCP SDK (`WithMCP`) | -1.7 MB | -1.8 MB | |
| without the OTLP exporter and the SDK (`telemetry`) | -2.7 MB | -2.1 MB | -3.8 ms |
| without all four | -10.2 MB | -8.3 MB | -5.2 ms |

With tracing on (`OTEL_EXPORTER_OTLP_ENDPOINT` set, every request sampled) the same service holds 22.0 MB, one
more goroutine and one more thread: the SDK is linked either way, but it only does work once it is configured.

## The opt-out build tags

A binary links every package its imports reach, whatever it calls, and the linker cannot drop a package that
is initialised at import: making the MCP bridge reachable only through `WithMCP` was tried and saved 115 KB of
1.7 MB. What does work is not importing the package, so `server` keeps two features behind build tags.

| Tag | A binary built with it does not link | What it does when the feature is asked for |
| --- | --- | --- |
| `gocommon_nospiffe` | `spiffe`, go-spiffe, go-jose | `GRAPH_SPIFFE_COLLECTOR_URL` set: graph's sender logs that the binary was built without it (first attempt, then every tenth) and keeps using the writer key, as while the Workload API is unreachable |
| `gocommon_nopyroscope` | `profiling`, the Pyroscope client | `PYROSCOPE_SERVER_ADDRESS` set: one log line, no profile is sent |

Without the tags nothing changes: the binary is the size it was to the byte (20,361,378 bytes), and its RSS is the same within the run-to-run spread. With both:
the minimal service is 19,083,426 bytes instead of 20,361,378 (-1.28 MB, -6.3%), links 38 modules instead of 43 and
468 packages instead of 503, and holds 17.95 MB of idle RSS instead of 19.24 MB (-1.29 MB, -6.7%; 0.1 MB of it private, the rest is
the executable's pages). Start-up, initialisation and goroutine counts do not change.

```dockerfile
RUN CGO_ENABLED=0 go build -trimpath -tags gocommon_nospiffe,gocommon_nopyroscope -ldflags='-s -w' -o /app/server .
```

`scripts/linkcheck.sh` (the `validate` step) holds both halves with `go list -deps`: the feature's packages are
absent from the reference service built with the tag and present in one built without it, so a refactor cannot
turn a tag into a no-op or make a default build lose a feature. `scripts/linkcheck.sh --size` also builds both and
prints the sizes. A new opt-out is one tag, one pair of files and one line in `scripts/linkcheck.txt`.

## What is not here

- **Nested modules and separate repositories change nothing at run time.** A binary links packages, not modules:
  the same import graph links the same code whichever module a package lives in. They change the consumer's `go.sum`
  and what `go list -m all` prints, and a consumer that does not import a heavy package already has none of it in
  its `go.mod` (docs/adr/0002-dependency-budget.md measured it).
- **The MCP SDK and the OTLP exporter are not opt-outs here.** 129 of the 131 repositories that import `server`
  call `WithMCP`, and tracing is on wherever the vault credential or an OTLP endpoint is configured. A tag for
  either is a few lines (the scratch builds above are that), but it saves nothing for the services that use it.
- **Numbers are for this binary on this host.** They move a little with the Go patch release and the CPU; the
  ratios between the rows are what to trust.
