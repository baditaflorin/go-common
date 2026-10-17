# Who imports what

Snapshot: **2026-10-17**, go-common 0.102.71. For each importable package: how many
repositories import it. The point is to make a removal decision with numbers
instead of a feeling. Read the caveats first: the numbers can show that a package
is used, but not that it is unused.

**This is a lower bound.** [docs/api-policy.md](api-policy.md) says roughly a
hundred services pin go-common. This snapshot saw **59** repositories that import
at least one package from non-test code, because it can only see the services
cloned on the machine it ran on. A `0` means "no importer found in the clones that were scanned", never
"nobody uses it". A package that was imported by one scanned repository is used.

## How it was counted

- **Repositories scanned**: every Go file under one directory of local clones,
  including the repositories nested inside others. `vendor/` is skipped.
- **A repository counts once.** The unit is the nearest directory with a `.git`.
  Dated copies, worktrees, rollback and research checkouts and forks of one
  service are collapsed into one repository by their origin URL, and copies of
  go-common itself are not consumers.
- **An import is an import line** (`import "..."`, with or without an alias, `_`
  or `.`), not a string or a comment that names a package.
- **Repos** counts repositories that import the package from a non-test `.go` file.
  **Tests only** counts repositories that import it only from `_test.go` files.
  A package with `0` in both columns has no importer at all in the scanned clones.
- **go-common** counts packages of this module that import it from non-test code
  (direct imports, including `scripts/`); **tests** counts the ones that import
  it only from tests. A package with no service importer that go-common itself
  imports (for example through `server`) still reaches every service that uses
  that package, so it cannot be removed on the strength of a `0` in **Repos**.
- Versions are not considered. Services pin different go-common versions, and a
  removed package only breaks a service when it bumps.

## How to refresh

```bash
scripts/package-usage.sh /path/to/the/directory/with/the/clones
```

It only reads files (ripgrep if installed, else grep; no Go toolchain, no
network), prints one tab-separated line per package (`package`, repos, tests only,
go-common, go-common tests) and, on stderr, the number of repositories it saw. It
prints counts and never a repository name. Paste the numbers over the table below,
update the snapshot date, and keep the notes honest.

## Summary

Of the 72 importable packages:

| Repositories importing it from non-test code | Packages |
| --- | --- |
| 10 or more | 2 (`config`, `server`) |
| 3 to 9 | 10 |
| 1 or 2 | 20 |
| none, only tests import it | 2 (both test helpers) |
| none | 38: 20 are imported by other go-common packages, 2 by go-common's tests only, 16 by nothing |

Of the 16 packages that nothing imports, 3 are opt-in and new (`bootgate`,
`chromeguard`, `egressproxy`), `meshclient` is new in 0.102.71, `obs` is an opt-in
debug server and 3 are test helpers. The rest (`dataformat`, `dbx`, `jsbundle`,
`nullify`, `pentest`, `qrterm`, `shutdown`, `wordbound`) are the ones to ask
questions about: see "Before removing a package".

## Per package

| Package | Repos | Tests only | go-common | go-common tests | Notes |
| --- | ---: | ---: | ---: | ---: | --- |
| [`agent`](../agent/) | 1 | 0 | 1 | 0 |  |
| [`apikey`](../apikey/) | 7 | 0 | 4 | 0 |  |
| [`backoffcoord`](../backoffcoord/) | 0 | 0 | 1 | 0 | Linked in through other go-common packages. |
| [`bootgate`](../bootgate/) | 0 | 0 | 0 | 0 | Opt-in, new (2026-10). |
| [`browserheaders`](../browserheaders/) | 1 | 0 | 0 | 0 |  |
| [`cache`](../cache/) | 1 | 0 | 0 | 0 |  |
| [`cgroup`](../cgroup/) | 0 | 0 | 2 | 0 | Linked in through other go-common packages. |
| [`chromeguard`](../chromeguard/) | 0 | 0 | 0 | 0 | Opt-in, new (2026-10). |
| [`circuitbreaker`](../circuitbreaker/) | 0 | 0 | 2 | 1 | Linked in through other go-common packages. |
| [`client`](../client/) | 2 | 0 | 1 | 0 | Partly deprecated (`Client`, `New`, `Post`); `Fetch` and the capability types are in use. |
| [`clock`](../clock/) | 0 | 0 | 3 | 1 | Linked in through other go-common packages. |
| [`compat`](../compat/) | 0 | 0 | 1 | 0 | The library behind `scripts/compatcheck`; for a service to call at start-up or in a test. |
| [`config`](../config/) | 52 | 1 | 1 | 0 |  |
| [`credentiallease`](../credentiallease/) | 1 | 0 | 0 | 1 | One adopter (added 2026-10). |
| [`credentiallease/broker`](../credentiallease/broker/) | 1 | 0 | 0 | 0 | One adopter (added 2026-10). |
| [`dataformat`](../dataformat/) | 0 | 0 | 0 | 0 | No importer found. The only user of two third-party parsers (TOML, YAML). |
| [`dbx`](../dbx/) | 0 | 0 | 0 | 0 | No importer found (added 2026-09). The only user of `pgx` and `otelsql`. |
| [`degraded`](../degraded/) | 0 | 0 | 2 | 0 | Linked in through other go-common packages. |
| [`depcheck`](../depcheck/) | 0 | 0 | 2 | 0 | Linked in through other go-common packages. |
| [`deploymentauthorization`](../deploymentauthorization/) | 1 | 0 | 1 | 0 | One adopter (added 2026-10). |
| [`deploymentintent`](../deploymentintent/) | 1 | 0 | 2 | 0 | One adopter (added 2026-10). |
| [`deploymentlease`](../deploymentlease/) | 1 | 0 | 0 | 0 | One adopter (added 2026-10). |
| [`egress`](../egress/) | 0 | 0 | 2 | 0 | Linked in through other go-common packages. |
| [`egressproxy`](../egressproxy/) | 0 | 0 | 0 | 0 | Opt-in, new (2026-10). |
| [`env`](../env/) | 8 | 0 | 9 | 0 |  |
| [`errors`](../errors/) | 0 | 0 | 2 | 0 | Linked in through other go-common packages. No service imports it directly. |
| [`fleetfetch`](../fleetfetch/) | 4 | 0 | 2 | 0 |  |
| [`graph`](../graph/) | 4 | 0 | 8 | 0 |  |
| [`header`](../header/) | 4 | 0 | 5 | 2 |  |
| [`jsbundle`](../jsbundle/) | 0 | 0 | 0 | 0 | No importer found, but services that may use it were not available to scan. |
| [`ledger`](../ledger/) | 1 | 0 | 0 | 0 |  |
| [`loadshed`](../loadshed/) | 0 | 0 | 3 | 0 | Linked in through other go-common packages. |
| [`logx`](../logx/) | 0 | 0 | 1 | 1 | Opt-in (`LOG_FORMAT=json`); started by `server`. |
| [`meshclient`](../meshclient/) | 0 | 0 | 0 | 0 | New (2026-10); for calls to sibling services. |
| [`meshresult`](../meshresult/) | 2 | 0 | 0 | 1 |  |
| [`metrics`](../metrics/) | 0 | 0 | 2 | 0 | Linked in through other go-common packages. |
| [`middleware`](../middleware/) | 2 | 0 | 4 | 0 |  |
| [`nullify`](../nullify/) | 0 | 0 | 0 | 0 | No importer found; `docs/outcomes.md` points to it. |
| [`obs`](../obs/) | 0 | 0 | 0 | 0 | Opt-in debug server (localhost only). |
| [`openapi`](../openapi/) | 2 | 0 | 1 | 0 |  |
| [`pagination`](../pagination/) | 2 | 0 | 0 | 0 |  |
| [`pentest`](../pentest/) | 0 | 0 | 0 | 0 | No importer found, but services that may use it were not available to scan. |
| [`policyeval`](../policyeval/) | 0 | 0 | 1 | 0 | Linked in through other go-common packages. |
| [`profiling`](../profiling/) | 0 | 0 | 1 | 0 | Opt-in; started by `server`. |
| [`promx`](../promx/) | 2 | 0 | 4 | 0 |  |
| [`proxysupplier`](../proxysupplier/) | 2 | 0 | 2 | 0 |  |
| [`qrterm`](../qrterm/) | 0 | 0 | 0 | 0 | No importer found (added 2026-09). The only user of `go-qrcode`. |
| [`ratecoord`](../ratecoord/) | 0 | 0 | 1 | 0 | Linked in through other go-common packages. |
| [`reqstats`](../reqstats/) | 0 | 0 | 1 | 0 | Linked in through other go-common packages. |
| [`resilience`](../resilience/) | 0 | 0 | 1 | 0 | New; used by `meshclient`. |
| [`response`](../response/) | 9 | 0 | 6 | 1 |  |
| [`retry`](../retry/) | 1 | 0 | 1 | 1 |  |
| [`runtimetune`](../runtimetune/) | 0 | 0 | 2 | 0 | Applied by `server`. |
| [`safehttp`](../safehttp/) | 8 | 1 | 7 | 0 |  |
| [`secrets`](../secrets/) | 2 | 0 | 3 | 0 |  |
| [`selftest`](../selftest/) | 9 | 0 | 2 | 0 |  |
| [`server`](../server/) | 52 | 1 | 1 | 0 |  |
| [`shutdown`](../shutdown/) | 0 | 0 | 0 | 0 | Deprecated: use `server`. No importer found. |
| [`spiffe`](../spiffe/) | 0 | 0 | 2 | 0 | Opt-in; used by `credentiallease` and `deploymentlease`. |
| [`telemetry`](../telemetry/) | 5 | 0 | 3 | 0 |  |
| [`testhelpers/fakeexec`](../testhelpers/fakeexec/) | 0 | 1 | 0 | 0 | Test helper. |
| [`testhelpers/fakegraph`](../testhelpers/fakegraph/) | 0 | 0 | 0 | 0 | Test helper. No importer found. |
| [`testhelpers/fakekey`](../testhelpers/fakekey/) | 0 | 0 | 0 | 0 | Test helper. No importer found. |
| [`testhelpers/httpmock`](../testhelpers/httpmock/) | 0 | 0 | 0 | 0 | Test helper. No importer found. |
| [`testhelpers/leakcheck`](../testhelpers/leakcheck/) | 0 | 0 | 0 | 4 | Test helper (new); used by go-common's own tests. |
| [`testhelpers/ssrfcorpus`](../testhelpers/ssrfcorpus/) | 0 | 0 | 0 | 3 | Test helper (new); used by go-common's own tests. |
| [`testhelpers/testclock`](../testhelpers/testclock/) | 0 | 1 | 0 | 0 | Test helper. |
| [`ua`](../ua/) | 5 | 0 | 0 | 1 |  |
| [`validate`](../validate/) | 2 | 0 | 0 | 0 |  |
| [`version`](../version/) | 1 | 0 | 0 | 0 |  |
| [`wordbound`](../wordbound/) | 0 | 0 | 0 | 0 | No importer found (added 2026-08). |
| [`workpool`](../workpool/) | 0 | 0 | 1 | 0 | Linked in through other go-common packages. |

## Before removing a package

A `0` here is where the question starts, not where it ends. [docs/api-policy.md](api-policy.md)
says what a removal needs (a `Deprecated:` paragraph, two minor releases, a
`**BREAKING:**` changelog entry). Before even deprecating, check that:

1. the package has a replacement in go-common that you can name, or the package
   is not worth keeping;
2. nothing in go-common imports it (a deprecated package that go-common still
   imports makes go-common's own lint fail with SA1019);
3. a census of **all** services, from the service registry and not from whatever
   happens to be cloned, shows no importer. Ask the services that are named like
   the package (a `pentest-*` or `jsbundle-*` service probably uses `pentest`
   or `jsbundle`) directly.
