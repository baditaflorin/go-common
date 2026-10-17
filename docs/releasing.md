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
  changes can claim the same number while both are open (it happened to 0.102.66);
  whichever merges second fails the check and renumbers.
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

## What each script checks

| Script | Checks |
| --- | --- |
| `scripts/release-check.sh` | The claimed version is well formed, unique and the next free one |
| `scripts/apidiff-check.sh` | An incompatible API change is declared BREAKING |
| `scripts/bench-gate.sh` | A hot-path benchmark did not start allocating more |
| `scripts/backport.sh` | Nothing: it prepares a backport branch |
