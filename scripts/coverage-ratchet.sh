#!/usr/bin/env bash
# Per-package coverage ratchet.
#
#   scripts/coverage-ratchet.sh                 # run `go test -cover ./...`; fail when a package is below its floor
#   scripts/coverage-ratchet.sh --update        # raise coverage/floors.txt to what is measured (never lowers)
#   scripts/coverage-ratchet.sh --from FILE     # judge the output of `go test -cover ./...` in FILE ("-": stdin)
#
#   COVERAGE_ADVISORY=1   print the table and the verdict, but always exit 0
#   COVERAGE_MARGIN=2     points below the measured value that --update sets a floor (default 2)
#   COVERAGE_FLOORS=...   the floors file (default coverage/floors.txt)
#
# coverage/floors.txt holds one line per tested package: "<package> <floor in percent>".
# A package fails when its statement coverage is below its floor, or has no floor yet (a
# new package has to be tracked: run --update and commit the file). A floor is set at the
# measured coverage minus the margin, rounded down, and only ever goes up: a change that
# adds tests runs --update to keep the gain, and one that lowers a floor edits the file
# by hand, where review sees it. See coverage/README.md.
#
# `go test -cover` prints one decimal, and this script compares in tenths of a percent so
# that 82.0 against a floor of 82 is a pass, not a rounding accident. Packages without
# test files or without statements are not judged. Exit 0: every package is at or above
# its floor. Exit 1: one is not, or `go test` failed. Exit 2: it could not run.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

floors="${COVERAGE_FLOORS:-coverage/floors.txt}"
margin="${COVERAGE_MARGIN:-2}"
advisory="${COVERAGE_ADVISORY:-0}"
update=0
from=""

die() { echo "coverage-ratchet: $*" >&2; exit 2; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --update) update=1 ;;
    --from) shift; [ "$#" -gt 0 ] || die "--from needs a file (or - for stdin)"; from="$1" ;;
    -h | --help) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument '$1' (see --help)" ;;
  esac
  shift
done
[[ "$margin" =~ ^[0-9]+$ ]] || die "COVERAGE_MARGIN must be a whole number of points, not '${margin}'"
[ -f go.mod ] || die "no go.mod here; run it from a go-common checkout"
module="$(awk '$1 == "module" { print $2; exit }' go.mod)"

out="$(mktemp)"
trap 'rm -f "$out" "${out}.new"' EXIT
test_rc=0
if [ -n "$from" ]; then
  if [ "$from" = "-" ]; then cat >"$out"; else cp "$from" "$out" || die "cannot read ${from}"; fi
else
  go test -count=1 -cover ./... 2>&1 | tee "$out" || test_rc="${PIPESTATUS[0]}"
  echo
fi

# measured: "<package> <tenths of a percent>" for every package whose tests ran with
# statements, "<package> notests" for one without test files. `ok  <pkg> <time> coverage:
# 87.5% of statements` and the tab-led `<pkg> coverage: 0.0% of statements` of a package
# with no tests are the two forms; `[no statements]` and FAIL lines are not measurements.
measured="$(awk -v mod="${module}/" '
  function rel(p) { sub("^" mod, "", p); return (p == "") ? "." : p }
  function tenths(s,   a) { split(s, a, "."); return a[1] * 10 + substr(a[2] "0", 1, 1) }
  /^ok[ \t]/ && match($0, /coverage: [0-9.]+% of statements/) {
    pct = substr($0, RSTART + 10, RLENGTH - 10); sub(/%.*/, "", pct)
    print rel($2), tenths(pct); next
  }
  /^[ \t]+[^ \t]+[ \t]+coverage: [0-9.]+% of statements/ { print rel($1), "notests" }
' "$out" | sort)"

failed_pkgs="$(awk -v mod="${module}/" '/^FAIL[ \t]/ { p = $2; sub("^" mod, "", p); print p }' "$out" | sort -u | paste -sd' ' -)"

# Floors: "<package> <points>" lines; blank lines and #-comments are skipped.
declare -A floor_of=()
if [ -f "$floors" ]; then
  while read -r pkg value _; do
    case "$pkg" in '' | '#'*) continue ;; esac
    [[ "$value" =~ ^[0-9]+(\.[0-9])?$ ]] || die "${floors}: '${pkg} ${value}' is not '<package> <percent>'"
    whole="${value%%.*}"
    frac="${value#"$whole"}"; frac="${frac#.}"
    floor_of["$pkg"]=$((whole * 10 + ${frac:-0}))
  done <"$floors"
fi

fmt() { printf '%d.%d' "$(($1 / 10))" "$(($1 % 10))"; }
# A floor is printed as a whole number unless it was given a decimal by hand.
ffmt() { if [ $(($1 % 10)) -eq 0 ]; then printf '%d' "$(($1 / 10))"; else fmt "$1"; fi; }

below=0 fresh=0 stale=0 ok=0 raisable=0 untested=()
declare -A seen=() want=()
rows=""
while read -r pkg value; do
  [ -n "$pkg" ] || continue
  if [ "$value" = "notests" ]; then
    # No test files. A package with a floor that lost its tests is at 0.0%, below it;
    # one that never had a floor is just not judged.
    if [ "${floor_of[$pkg]:-0}" -gt 0 ]; then value=0; else untested+=("$pkg"); continue; fi
  fi
  seen["$pkg"]=1
  candidate=$(((value - margin * 10) / 10))
  if [ $((value - margin * 10)) -lt 0 ]; then candidate=0; fi
  want["$pkg"]=$candidate
  if [ -z "${floor_of[$pkg]:-}" ]; then
    fresh=$((fresh + 1))
    rows+="$(printf '%-36s %6s %7s  %s' "$pkg" "-" "$(fmt "$value")" "NO FLOOR (new package: run scripts/coverage-ratchet.sh --update)")"$'\n'
  elif [ "$value" -lt "${floor_of[$pkg]}" ]; then
    below=$((below + 1))
    rows+="$(printf '%-36s %6s %7s  %s' "$pkg" "$(ffmt "${floor_of[$pkg]}")" "$(fmt "$value")" "BELOW FLOOR by $(fmt $((floor_of[$pkg] - value)))")"$'\n'
  else
    ok=$((ok + 1))
    note="ok"
    if [ $((candidate * 10)) -gt "${floor_of[$pkg]}" ]; then
      raisable=$((raisable + 1))
      note="ok (the floor can rise to ${candidate})"
    fi
    rows+="$(printf '%-36s %6s %7s  %s' "$pkg" "$(ffmt "${floor_of[$pkg]}")" "$(fmt "$value")" "$note")"$'\n'
  fi
done <<<"$measured"

printf '%-36s %6s %7s  %s\n' PACKAGE FLOOR NOW STATUS
printf '%s' "$rows"
echo
for pkg in "${!floor_of[@]}"; do
  if [ -z "${seen[$pkg]:-}" ]; then
    stale=$((stale + 1))
    echo "coverage-ratchet: warning: ${floors} has ${pkg}, which was not measured (removed, renamed, or its tests are gone); delete the line if the package is gone"
  fi
done
total=$((ok + below + fresh))
echo "coverage-ratchet: ${total} packages: ${ok} at or above their floor, ${below} below, ${fresh} without a floor; ${raisable} could raise theirs; ${#untested[@]} have no tests (not judged)"

if [ "$test_rc" -ne 0 ]; then
  echo "coverage-ratchet: go test failed (exit ${test_rc})${failed_pkgs:+ in ${failed_pkgs}}, so coverage is not judged; fix the tests first" >&2
  exit 1
fi
if [ "$total" -eq 0 ]; then
  die "no coverage lines found in the go test output"
fi

if [ "$update" -eq 1 ]; then
  if [ "$below" -gt 0 ]; then
    echo "coverage-ratchet: ${below} package(s) are below their floor; --update raises floors and never lowers them, so nothing was written. Restore the coverage, or lower the floor by hand where review sees it." >&2
    exit 1
  fi
  declare -A next=()
  for pkg in "${!floor_of[@]}"; do next["$pkg"]="$((floor_of[$pkg]))"; done
  changed=0
  for pkg in $(printf '%s\n' "${!want[@]}" | sort); do
    new=$((want[$pkg] * 10))
    old="${next[$pkg]:-}"
    if [ -z "$old" ]; then
      next["$pkg"]=$new; changed=1
      echo "coverage-ratchet: ${pkg}: new floor $((new / 10))"
    elif [ "$new" -gt "$old" ]; then
      next["$pkg"]=$new; changed=1
      echo "coverage-ratchet: ${pkg}: floor $(ffmt "$old") -> $((new / 10))"
    fi
  done
  if [ "$changed" -eq 0 ]; then
    echo "coverage-ratchet: every floor is already at the measured value minus ${margin}; ${floors} is unchanged"
    exit 0
  fi
  {
    # Keep the file's comment header (everything before the first package line).
    if [ -f "$floors" ]; then awk '/^[ \t]*($|#)/ { print; next } { exit }' "$floors"; fi
    for pkg in $(printf '%s\n' "${!next[@]}" | sort); do
      v="${next[$pkg]}"
      echo "${pkg} $(ffmt "$v")"
    done
  } >"${out}.new"
  mkdir -p "$(dirname "$floors")"
  cp "${out}.new" "$floors"
  echo "coverage-ratchet: wrote ${floors}"
  exit 0
fi

if [ $((below + fresh)) -gt 0 ]; then
  if [ "$advisory" = "1" ]; then
    echo "coverage-ratchet: COVERAGE_ADVISORY=1, so ${below} below and ${fresh} untracked package(s) do not fail this run"
    exit 0
  fi
  cat >&2 <<'MSG'
coverage-ratchet: FAIL. A package has less coverage than its floor in coverage/floors.txt,
or is not tracked there yet. Add the tests that restore the coverage. For a new package,
run scripts/coverage-ratchet.sh --update and commit coverage/floors.txt. See coverage/README.md.
MSG
  exit 1
fi
echo "coverage-ratchet: no package is below its floor"
