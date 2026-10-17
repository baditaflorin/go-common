# Shared lint config for go-common consumers

`lint/golangci.yml` is a small golangci-lint v2 config that turns the "use the
go-common primitive" rules into checks. It adds, on top of golangci's standard
set:

| Rule | Why |
| --- | --- |
| `staticcheck` SA1019 (already in the standard set) | Flags use of anything go-common marks `// Deprecated:`, so a deprecation reaches your CI before the removal does |
| `forbidigo`: `http.Get`, `Head`, `Post`, `PostForm`, `http.DefaultClient` | They use the default client: no timeout and no SSRF guard. Use `safehttp.NewClient` for the public internet, or an `http.Client` with a `Timeout` for sibling services |
| `forbidigo`: `http.ListenAndServe`, `ListenAndServeTLS` | No timeouts and no graceful drain. Use `go-common/server` |
| `noctx` | `http.NewRequest` and friends without a context cannot be cancelled |
| `bodyclose` | A response body that is never closed leaks the connection |

Test files are exempt from `forbidigo` and `noctx`.

## Use it

golangci-lint v2 has no `extends`, so point `--config` at the file inside the
go-common version your `go.mod` pins. The rules then move with the dependency,
and no network is needed beyond the module cache:

```bash
golangci-lint run \
  --config "$(go list -m -f '{{.Dir}}' github.com/baditaflorin/go-common)/lint/golangci.yml" ./...
```

## Adopting it in an old service

Do not fix everything in one go. Ratchet: fail only on findings in code you
changed.

```bash
golangci-lint run --new-from-rev=origin/main \
  --config "$(go list -m -f '{{.Dir}}' github.com/baditaflorin/go-common)/lint/golangci.yml" ./...
```

## Change a rule

Edit `lint/golangci.yml`, then add or adjust a case in `lint/fixture/` and run
`lint/selftest.sh`. The fixture is a deliberately bad nested module; the script
fails if a rule stops firing, or if the clean function starts to.
