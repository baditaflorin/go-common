#!/usr/bin/env bash
# Tests scripts/coverage-ratchet.sh with a fake `go` that prints a canned `go test -cover ./...`,
# so it needs no toolchain and takes a moment. It checks that
#   - --update writes a floor of the measured coverage minus the margin, rounded down and never
#     below 0, in sorted order, and never lowers a floor or drops a line;
#   - a package below its floor (by a tenth of a point, too) fails, and --update then writes nothing;
#   - a tested package without a floor fails until --update adds it;
#   - packages without test files or statements are not judged, but a package with a floor whose
#     tests are gone is below it;
#   - a failing `go test` fails the run and blocks --update;
#   - COVERAGE_ADVISORY=1 reports without failing, and --from judges a saved output without go.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"

# The fake go prints $FAKE_OUT and exits $FAKE_RC (default 0).
cat >"$tmp/bin/go" <<'FAKE'
#!/usr/bin/env bash
cat "$FAKE_OUT"
exit "${FAKE_RC:-0}"
FAKE
chmod +x "$tmp/bin/go"

repo="$tmp/repo"
mkdir -p "$repo/scripts" "$repo/coverage"
cp "$root/scripts/coverage-ratchet.sh" "$repo/scripts/"
printf 'module example.com/m\n\ngo 1.26.0\n' >"$repo/go.mod"
cd "$repo"
floors="coverage/floors.txt"

# What a real run prints: the ok form with a time or (cached), `[no statements]`, the tab-led
# no-test-files form, and a package nested in another.
base_output() {
  cat <<'EOF'
ok  	example.com/m/a	0.003s	coverage: 87.5% of statements
ok  	example.com/m/b	0.014s	coverage: 58.9% of statements
ok  	example.com/m/d/e	(cached)	coverage: 100.0% of statements
ok  	example.com/m/f	0.004s	coverage: 2.0% of statements
ok  	example.com/m/g	0.004s	coverage: 1.5% of statements
ok  	example.com/m/h	0.004s	coverage: 82.0% of statements
ok  	example.com/m/i	0.015s	coverage: 34.9% of statements
ok  	example.com/m/j	0.060s	coverage: 67.6% of statements
ok  	example.com/m/header	0.006s	coverage: [no statements]
	example.com/m/scripts/tool		coverage: 0.0% of statements
EOF
}
export FAKE_OUT="$tmp/out.txt"
export PATH="$tmp/bin:$PATH"
base_output >"$FAKE_OUT"
expected_floors='a 85
b 56
d/e 98
f 0
g 0
h 80
i 32
j 65'

fail=0
# check <name> <wanted exit code> <pattern the output must contain, or "-"> <command...>
check() {
  local name="$1" want_exit="$2" pattern="$3" exit_code=0
  shift 3
  "$@" >"$tmp/run.log" 2>&1 || exit_code=$?
  if [ "$exit_code" -ne "$want_exit" ]; then
    echo "FAIL: $name: exit $exit_code, want $want_exit" >&2
    sed 's/^/    /' "$tmp/run.log" >&2
    fail=1
  elif [ "$pattern" != "-" ] && ! grep -qF -- "$pattern" "$tmp/run.log"; then
    echo "FAIL: $name: the output lacks '$pattern'" >&2
    sed 's/^/    /' "$tmp/run.log" >&2
    fail=1
  else
    echo "ok: $name"
  fi
}
# floors_are <name> <expected file body>: the floors file holds exactly these lines (after its comments).
floors_are() {
  local got
  got="$(grep -vE '^[[:space:]]*(#|$)' "$floors" 2>/dev/null || true)"
  if [ "$got" != "$2" ]; then
    echo "FAIL: $1: $floors holds" >&2
    printf '%s\n' "$got" | sed 's/^/    /' >&2
    echo "  want" >&2
    printf '%s\n' "$2" | sed 's/^/    /' >&2
    fail=1
  else
    echo "ok: $1"
  fi
}
run() { scripts/coverage-ratchet.sh "$@"; }

rm -f "$floors"
check "a package with no floor fails" 1 "NO FLOOR" run
check "--update writes the first floors" 0 "wrote coverage/floors.txt" run --update
floors_are "floors are the measured value minus 2, rounded down, never below 0" "$expected_floors"
check "every package passes at its own floor" 0 "no package is below its floor" run
check "the table names packages, floor and measured value" 0 "d/e" run
grep -qE '^b +56 +58\.9 +ok' "$tmp/run.log" || { echo "FAIL: the table row for b is not '<floor> <now> ok'" >&2; sed 's/^/    /' "$tmp/run.log" >&2; fail=1; }
check "packages without tests or statements are not judged" 0 "1 have no tests" run

# A tenth of a point below a floor fails. 58.9 against 56: drop b to 55.9.
sed -i.bak 's/58\.9%/55.9%/' "$FAKE_OUT" && rm -f "$FAKE_OUT.bak"
cp "$floors" "$tmp/floors.before"
check "a package below its floor fails and is named" 1 "BELOW FLOOR by 0.1" run
check "--update refuses to write while a package is below its floor" 1 "nothing was written" run --update
cmp -s "$floors" "$tmp/floors.before" && echo "ok: the floors file is untouched" || { echo "FAIL: --update changed the floors of a failing run" >&2; fail=1; }
check "COVERAGE_ADVISORY=1 reports a failure without failing" 0 "BELOW FLOOR" env COVERAGE_ADVISORY=1 scripts/coverage-ratchet.sh
base_output >"$FAKE_OUT"

# Equal to the floor passes: h is 82.0 with a floor of 80, so give it a floor of exactly 82.
sed -i.bak 's/^h 80$/h 82/' "$floors" && rm -f "$floors.bak"
check "coverage equal to the floor passes" 0 "no package is below its floor" run
sed -i.bak 's/82\.0%/81.9%/' "$FAKE_OUT" && rm -f "$FAKE_OUT.bak"
check "81.9 against a floor of 82 fails" 1 "BELOW FLOOR by 0.1" run
base_output >"$FAKE_OUT"
sed -i.bak 's/^h 82$/h 80/' "$floors" && rm -f "$floors.bak"

# A gain: a goes from 87.5 to 91.0. The run passes and says the floor can rise; --update raises it.
sed -i.bak 's/87\.5%/91.0%/' "$FAKE_OUT" && rm -f "$FAKE_OUT.bak"
check "a gain passes and says the floor can rise" 0 "the floor can rise to 89" run
check "--update raises the floor" 0 "a: floor 85 -> 89" run --update
floors_are "only the raised floor changed" "$(printf '%s' "$expected_floors" | sed 's/^a 85$/a 89/')"

# Never lowers: a is back at 87.5 (floor 89 is now above it, so that fails); a hand-set floor that is
# above measured-2 but not above measured is kept by --update.
base_output >"$FAKE_OUT"
printf '%s\n' "$expected_floors" | sed 's/^a 85$/a 86/' >"$tmp/floors.body"
{ echo '# header kept'; cat "$tmp/floors.body"; } >"$floors"
check "a floor above measured-2 still passes below the measured value" 0 "no package is below its floor" run
check "--update does not lower a floor" 0 "unchanged" run --update
floors_are "the hand-set floor survives --update" "$(printf '%s' "$expected_floors" | sed 's/^a 85$/a 86/')"
grep -q '^# header kept$' "$floors" || { echo "FAIL: --update dropped the comment header" >&2; fail=1; }

# A new package: z is measured and has no floor.
printf 'ok  \texample.com/m/z\t0.004s\tcoverage: 50.0%% of statements\n' >>"$FAKE_OUT"
check "a new package fails until it has a floor" 1 "z " run
check "COVERAGE_ADVISORY=1 lets a new package through" 0 "do not fail this run" env COVERAGE_ADVISORY=1 scripts/coverage-ratchet.sh
check "--update adds the new package" 0 "z: new floor 48" run --update
check "the new package now passes" 0 "no package is below its floor" run
base_output >"$FAKE_OUT"

# A stale floor (a package that was removed) warns, and --update keeps the line.
printf 'gone 70\n' >>"$floors"
check "a floor for a package that is gone only warns" 0 "gone, which was not measured" run
run --update >/dev/null 2>&1 || true
grep -q '^gone 70$' "$floors" && echo "ok: --update keeps a stale line" || { echo "FAIL: --update dropped the stale line" >&2; fail=1; }
sed -i.bak '/^gone 70$/d;/^z 48$/d' "$floors" && rm -f "$floors.bak"

# A package with a floor whose tests are gone is at 0, which is below it.
sed -i.bak 's#^ok  	example.com/m/j	.*#	example.com/m/j		coverage: 0.0% of statements#' "$FAKE_OUT" && rm -f "$FAKE_OUT.bak"
check "a package whose tests are gone is below its floor" 1 "BELOW FLOOR" run
base_output >"$FAKE_OUT"

# go test failing fails the run, and blocks --update.
{ base_output; printf 'FAIL\texample.com/m/b\t0.100s\n'; } >"$FAKE_OUT"
cp "$floors" "$tmp/floors.before"
FAKE_RC=1 check "a failing go test fails the run" 1 "go test failed (exit 1) in b" run
FAKE_RC=1 check "--update is refused after a failing go test" 1 "go test failed" run --update
cmp -s "$floors" "$tmp/floors.before" && echo "ok: the floors file is untouched by a failing go test" || { echo "FAIL: a failing go test changed the floors" >&2; fail=1; }
base_output >"$FAKE_OUT"

# --from judges a saved output without running go.
check "--from FILE judges a saved output (go is not on PATH)" 0 "no package is below its floor" \
  env PATH="/usr/bin:/bin" bash scripts/coverage-ratchet.sh --from "$FAKE_OUT"
check "--from - reads standard input" 0 "no package is below its floor" \
  bash -c 'env PATH="/usr/bin:/bin" scripts/coverage-ratchet.sh --from - <"$FAKE_OUT"'
check "an empty output is not a pass" 2 "no coverage lines" env PATH="/usr/bin:/bin" scripts/coverage-ratchet.sh --from /dev/null

# The margin is a setting.
rm -f "$floors"
COVERAGE_MARGIN=5 check "COVERAGE_MARGIN=5 floors at measured minus 5" 0 "wrote" env COVERAGE_MARGIN=5 scripts/coverage-ratchet.sh --update
floors_are "the margin is applied" "$(printf 'a 82\nb 53\nd/e 95\nf 0\ng 0\nh 77\ni 29\nj 62\n')"

# Bad input.
printf 'a lots\n' >"$floors"
check "a malformed floors line is refused" 2 "is not '<package> <percent>'" run
check "an unknown argument is refused" 2 "unknown argument" run --nope
check "--help prints the usage" 0 "Per-package coverage ratchet" run --help

exit "$fail"
