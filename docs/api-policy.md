# go-common API and deprecation policy

go-common is a 0.x module that roughly a hundred services pin to different
versions, and it ships several times a day. This page says what a consumer can
rely on when it bumps, and what a change must do to be allowed to break that.

## What counts as API

- Every exported identifier of a non-`internal` package, including `testhelpers/*`.
- Defaults and behaviour that callers observe: a default timeout, whether a
  client fails open or closed, the shape of a returned error, HTTP status codes
  and JSON field names, log lines and metric names that dashboards or alerts match.
- Environment variables: their names, their meaning, and what happens when one
  is unset.

## Rules

1. **Additive changes** (new package, function, field, option, env var) ship in
   any release and need no ceremony.
2. **An incompatible API change** (removed or renamed identifier, changed
   signature, changed field type, removed package) must say so. The change's
   `CHANGELOG.md` entry needs a bullet starting with **BREAKING:** that names what
   callers have to change. CI enforces this, see below.
3. **A behaviour change** that no compiler can see (a changed default, a removed
   or repurposed env var, a different error) is breaking too. Mark it
   **BREAKING (behavioural):**. CI cannot detect these; the author and the
   reviewer have to.
   Every **BREAKING...** bullet ends with a `summary:` line (at most 200 characters)
   and, when the old behaviour can be had back without a release, an `off-switch:` line;
   [releasing.md](releasing.md) has the grammar, and CI refuses a bullet without them.
4. **Deprecate before you remove.** Keep the old API, add its replacement, and
   put a `// Deprecated: use X instead. Removal no earlier than v0.N.0.` paragraph
   in the doc comment. Keep it for at least two minor releases. Consumers that
   run staticcheck (the shared lint config in `lint/` does) get an SA1019 warning
   at the call site.
5. **Retire an env var in two steps**: first log a one-line warning when it is
   still set, and only later stop reading it. A silent removal cannot be audited,
   because there is no way to ask the fleet who still sets it.
6. **A behaviour change takes a minor bump** (`0.103.0`, not `0.102.67`), so that a
   consumer that follows patch releases only never takes one. CI enforces it:
   `scripts/release-check.sh` fails a change that claims a patch release (the same
   major and minor as the newest tag) and has a **BREAKING (behavioural):** bullet.
   The exception is a change a consumer can safely take in a patch, typically an
   urgent security fix where waiting for a minor leaves services exposed: mark the
   bullet **BEHAVIOURAL (patch-safe):**, with its `summary:` and a `reason:` line that
   says why a patch may carry it. It stays in the change feed like any other. The rule
   is about behaviour only; an API change marked **BREAKING:** is not gated by it.

## What CI checks

`scripts/apidiff-check.sh` compares the exported API of the change with its base
(the PR's target branch, or the previous commit on main) using `apidiff`. If it
finds an incompatible change and the change's `CHANGELOG.md` diff adds no bullet
that starts with `**BREAKING`, the build fails.

It does not catch behaviour changes (rule 3), and it does not check that a removed
identifier was deprecated first (rule 4). Those stay a review responsibility.

`scripts/release-check.sh` checks the form of what the author wrote: the fields above
on every **BREAKING...** bullet, and that `compat/changes.json`, which the feed of
changes between two versions is read from, is what `CHANGELOG.md` says.

Run it by hand on a fleet host (it type-checks the whole module):

```bash
scripts/apidiff-check.sh origin/main
```

## Why this exists

Releases are frequent and the module is 0.x, so version numbers carry no
compatibility signal. This gate makes the changelog the signal: a bump with no
`**BREAKING` bullet in its entries is API-compatible, which is what lets fleet-wide
dependency bumps be merged without a human reading every diff.
