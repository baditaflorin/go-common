# Releasing go-common

Every merged change is a release: it adds a `CHANGELOG.md` entry, and the merge
commit is tagged `vX.Y.Z`. This repository's CI does not create the tag (the
tag comes from outside CI), so the checks below are about picking the right
number and keeping old lines patched.

## Picking the version

- Add one entry to the top of `CHANGELOG.md`: `## [X.Y.Z] - YYYY-MM-DD`, with
  `### Added | Changed | Fixed | Removed` bullets. The changelog is narrative on
  purpose (what changed, why, how it was verified), so it is written by the
  author and not generated from commit subjects.
- X.Y.Z is the next free version: newer than the newest tag and than the entry
  below it. `scripts/release-check.sh` enforces this on every pull request. Two
  changes can claim the same number while both are open (it happened to 0.102.66,
  0.102.67 and 0.102.68 on the day this check was written); whichever merges second fails the check and renumbers.
- A change that is BREAKING (see [api-policy.md](api-policy.md)) should take a
  minor bump, so that consumers who follow patch releases only are not broken.
- A change that leaves `CHANGELOG.md` alone claims no release, and the check
  passes.

## Security fixes and old release lines

`compat/compat.json` holds the floor: the oldest version that has every security
fix go-common itself shipped (`min_safe`). Services pinned below it are reported by
`go run github.com/baditaflorin/go-common/scripts/compatcheck@latest -gomod go.mod`
and by anything that calls `compat.CheckGoMod`.

- **Raise the floor** in the same change that ships a security fix: set `min_safe`
  and `min_safe_date` to that release and add the fix to `reasons`. It is a
  claim about go-common's own fixes, not about its dependencies or the Go
  toolchain; those advisories are tracked in `scripts/govulncheck-allow.txt`.
- **Backport only when a service cannot move.** Most services should take the
  newest release. When one cannot (it is stuck on an old line for a reason that
  will take a while to fix), cut a backport:

  ```bash
  scripts/backport.sh 0.97 9d2dc81     # --dry-run shows the plan first
  ```

  It creates `release/0.97` from the newest `v0.97.x` tag in a separate worktree
  and cherry-picks the fix; it pushes nothing. Finish by hand as it prints: a
  changelog entry, a CI run, the tag, and a `"v0.97": "v0.97.1"` line in the
  manifest's `lines`, which makes the floor check judge that line by its own floor.
- Do not create `release/*` branches speculatively: every maintained line is a
  line to keep patching. Create one when a real service needs it.

## Forward compatibility: the next `go` line

Go takes the default of each `GODEBUG` setting from the `go` line of the module
being built, not from the toolchain. go-common says `go 1.26.0`, so its tests
run with the 1.26 defaults on a 1.27 image as well, and a default Go
changes for a newer line cannot fail anything until a service raises its own `go`
line (which then breaks that service, not go-common's CI). An example, from when
the minimum was 1.25: Go 1.26 flipped `urlstrictcolons`, so `url.Parse` rejects
`http://::1:3001/`, and a guard in `proxysupplier` that parsed such a URL failed
open on any service built with `go 1.26` (fixed in 0.103.1). Nothing in
go-common's CI showed it then; the `forward-compat` step exists so the next one does.

The `forward-compat` step (`scripts/forward-compat.sh`) is what shows it: it copies
the tree to a temporary directory, runs `go mod edit -go=<version>.0` there, then
`go vet ./...` and `go test -count=1 ./...`, and lists the packages and tests that
fail. The working tree is not touched, and the changed line is never committed. Run
it before any change that moves the minimum Go, and let CI run it on every change.

```bash
scripts/forward-compat.sh 1.27     # the Go on PATH must be 1.27 or newer: it never downloads a toolchain
scripts/forward-compat.sh          # the minor in FORWARD_COMPAT_GO_MINOR (scripts/tool-versions.sh)
```

It is CPU-heavy like the other test gates: use CI or a build host rather than a
laptop. The CI step runs in a `golang` image of the `FORWARD_COMPAT_GO_MINOR`
minor; when the minimum Go reaches that minor, the script refuses to run ("not
newer"), so raise `FORWARD_COMPAT_GO_MINOR` and the image in the same change.
`scripts/forward-compat-selftest.sh` (in the `validate` step) tests the script itself
with a fake `go`.

**When it fails**, the code is wrong for a newer Go, and a service on that line has
the same bug today or will have it soon:

1. Reproduce it on the copy: `FORWARD_COMPAT_KEEP=1 scripts/forward-compat.sh 1.27`
   prints where the copy is kept; `go test -run TestName ./pkg` there fails the same way.
2. Find the setting: [go.dev/doc/godebug](https://go.dev/doc/godebug) lists every
   setting and the Go version that changed its default. Putting one back for a single run,
   `GODEBUG=urlstrictcolons=0 go test -run TestName ./pkg`, tells you which one it is.
3. Fix the code so it is right under the new default (and, where it can be, the old
   one), with a test that fails on the old code. A `godebug` line in go-common's own
   go.mod would only hide the failure from go-common's tests: a service's behaviour
   follows the service's go.mod, not go-common's.
4. Only as a last resort, and only for a service that cannot be fixed yet, pin the old
   behaviour in that service's own `go.mod` (`godebug urlstrictcolons=0`), with a comment
   that names the reason and the date to remove it. A pin hides the bug; it does not fix it.

The step blocks: a change that fails it does not merge. It was first proposed as
informational (`failure: ignore`), because the `proxysupplier` failure it found was
still on main; it blocks now that the fix (0.103.1) has merged. Do not add
`failure: ignore` back to get past a failure: fix the code, as in step 3.

## What each script checks

| Script | Checks |
| --- | --- |
| `scripts/release-check.sh` | The claimed version is well formed, unique and the next free one |
| `scripts/forward-compat.sh` | `go vet` and the tests still pass with a newer `go` line in go.mod (run on a copy) |
| `scripts/apidiff-check.sh` | An incompatible API change is declared BREAKING |
| `scripts/bench-gate.sh` | A hot-path benchmark did not start allocating more |
| `scripts/backport.sh` | Nothing: it prepares a backport branch |
