#!/usr/bin/env bash
# Allocation gate for the hot-path benchmarks.
#
#   scripts/bench-gate.sh [base-ref]
#
# Runs every benchmark at the base (see scripts/lib/base-ref.sh) and at HEAD with
# a fixed iteration count, and fails when a benchmark's allocs/op grew by more
# than 10% AND by at least 2. ns/op and B/op are printed for information only.
#
# Why allocations and not time: for a fixed iteration count the number of
# allocations per operation is deterministic, so a regression is a fact. Time on
# a shared CI runner is not, so a time gate would cry wolf and get switched off.
# Read the ns/op column as a hint, and use `go test -bench -count=10` with
# benchstat on a quiet machine when it matters.
#
# BENCHTIME overrides the iteration count (default 2000x). A benchmark that
# exists only at HEAD is new and passes; one that disappears is reported.
#
# CPU-heavy (it compiles and runs every benchmark twice): run it in CI or on a
# build host, not on a laptop.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=scripts/lib/base-ref.sh
source scripts/lib/base-ref.sh
if ! base="$(resolve_base_commit "${1:-}")"; then
  echo "bench-gate: could not determine the base to compare with" >&2
  exit 1
fi

benchtime="${BENCHTIME:-2000x}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/base"
git archive "$base" | tar -x -C "$tmp/base"

# run_benchmarks <module dir> <output file>
run_benchmarks() {
  if ! (cd "$1" && go test -run '^$' -bench . -benchmem -benchtime="$benchtime" -count=1 ./... >"$2" 2>&1); then
    cat "$2" >&2
    echo "bench-gate: the benchmarks failed to run in $1" >&2
    return 1
  fi
}

# parse <go test output>: "<package>.<Benchmark> <allocs/op> <B/op> <ns/op>" per benchmark.
parse() {
  awk '
    /^pkg: / { pkg = $2 }
    /^Benchmark/ {
      name = $1; sub(/-[0-9]+$/, "", name)
      ns = ""; b = ""; a = ""
      for (i = 1; i <= NF; i++) {
        if ($i == "ns/op") ns = $(i - 1)
        if ($i == "B/op") b = $(i - 1)
        if ($i == "allocs/op") a = $(i - 1)
      }
      if (a != "") print pkg "." name, a, b, ns
    }' "$1"
}

echo "bench-gate: HEAD vs $(git rev-parse --short "$base"), ${benchtime} per benchmark"
run_benchmarks "$tmp/base" "$tmp/base.out" || exit 1
run_benchmarks "." "$tmp/head.out" || exit 1
parse "$tmp/base.out" >"$tmp/base.txt"
parse "$tmp/head.out" >"$tmp/head.txt"

if [ ! -s "$tmp/head.txt" ]; then
  echo "bench-gate: no benchmarks found at HEAD" >&2
  exit 1
fi

awk -v base_file="$tmp/base.txt" '
  BEGIN {
    while ((getline line < base_file) > 0) {
      split(line, f, " ")
      ba[f[1]] = f[2]; bb[f[1]] = f[3]; bn[f[1]] = f[4]
    }
  }
  {
    key = $1; a = $2; b = $3; ns = $4
    seen[key] = 1
    short = key; sub(/^.*go-common\//, "", short)
    if (!(key in ba)) {
      printf "NEW         %-52s %4d allocs/op %7d B/op %10.0f ns/op\n", short, a, b, ns
      next
    }
    status = "ok"
    if (a > ba[key] * 1.10 && a - ba[key] >= 2) { status = "REGRESSION"; bad++ }
    printf "%-11s %-52s %4d -> %-4d allocs/op   %7d -> %-7d B/op   %9.0f -> %-9.0f ns/op\n", status, short, ba[key], a, bb[key], b, bn[key], ns
  }
  END {
    for (key in ba) if (!(key in seen)) {
      short = key; sub(/^.*go-common\//, "", short)
      printf "GONE        %s (present at the base, missing at HEAD)\n", short
    }
    exit bad > 0 ? 1 : 0
  }' "$tmp/head.txt" | tee "$tmp/report.txt" || true # the verdict is read from the report below

if grep -q '^REGRESSION' "$tmp/report.txt"; then
  cat >&2 <<'MSG'

bench-gate: FAIL. allocs/op grew by more than 10% (and at least 2) on a hot path
listed above. Every service pays these costs on every request, so find the new
allocation (`go test -bench NAME -benchmem -memprofile mem.out`), or, if the
extra work is the point of the change, say so in the PR and CHANGELOG.md and
re-baseline by merging.
MSG
  exit 1
fi
echo "bench-gate: no allocation regressions"
