# Per-package coverage floors

`floors.txt` holds, for every tested package, the lowest statement coverage CI accepts
for it: one `<package> <percent>` per line, the package relative to the module
(`middleware`, `credentiallease/broker`). `scripts/coverage-ratchet.sh` runs
`go test -cover ./...`, prints a table and fails when a package is below its floor, or
has tests and no floor yet. It is a ratchet, not a target: a floor only goes up, and the
number is the measured coverage at the time minus 2 points (rounded down), so a run-to-run
wobble in a timing-dependent test does not turn a pull request red.

Line coverage says what ran, not what is asserted. It stops the per-request packages
(`middleware`, `server`, `response`, `openapi`) from losing their tests unnoticed; it does
not say the tests are good.

## Where it runs

CI runs it on every push to main (the `coverage` step), not on pull requests: it is another
full test run, about 450 CPU-seconds, on an agent shared with the fleet's other pull requests.
A pull request can therefore lower a package's coverage and only main turns red, so run it
yourself before a change that deletes or rewrites tests (on a build host; it runs the whole
suite):

```bash
scripts/coverage-ratchet.sh
```

## What to do

| Situation | Do |
| --- | --- |
| CI (main) says `BELOW FLOOR` | Add the tests that restore the coverage. Do not lower the floor to get green. |
| You added tests and coverage went up | `scripts/coverage-ratchet.sh --update`, commit `floors.txt`: it raises floors (new floor = measured minus 2, rounded down) and never lowers one. |
| You added a package with tests | The same `--update`: CI fails a tested package that has no floor. |
| You deleted or renamed a package | Delete its line (the run warns about it; it is not a failure). |
| A floor really has to go down (code removed along with its tests) | Edit `floors.txt` by hand and say why in the pull request: the diff is where review sees it. |

```bash
scripts/coverage-ratchet.sh                      # judge the tree (runs the whole test suite: use a build host)
scripts/coverage-ratchet.sh --from cover.txt     # judge a saved `go test -cover ./...` output, no run
COVERAGE_MARGIN=0 scripts/coverage-ratchet.sh --update   # keep no slack (default 2 points)
```

Packages with no test files, or no statements, are not judged.

## Switching it off

The `coverage` step in `.woodpecker.yml` sets `COVERAGE_ADVISORY`: `"1"` prints the table and
the verdict but never fails the step. Reverting the step and `coverage/` removes the gate;
nothing else reads them.
