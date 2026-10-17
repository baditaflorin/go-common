#!/usr/bin/env bash
# Run every Fuzz* target in the module for a bounded time each.
#
#   scripts/fuzz.sh [fuzztime]            # default 10s per target
#   PKGS="./wordbound ./safehttp" scripts/fuzz.sh 2m
#   FUZZ_PARALLEL=2 scripts/fuzz.sh 30s   # cap fuzz workers (default 4)
#
# `go test` already replays each target's seed corpus (and every file under
# testdata/fuzz) as ordinary tests, so a plain `go test ./...` is the cheap
# regression gate. This script is the part that actually mutates inputs.
#
# It is CPU-heavy: run it in CI or on a fleet host, not on a laptop. Nightly
# runs should use a long fuzztime (minutes), the per-PR smoke run a few seconds.
#
# When a target fails, Go writes the minimal crashing input to
# <pkg>/testdata/fuzz/<Target>/<hash>. Fix the bug, then COMMIT that file: it
# becomes a permanent regression seed that plain `go test` replays.
set -euo pipefail

fuzztime="${1:-10s}"
parallel="${FUZZ_PARALLEL:-4}"
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# `go test -list` prints each package's matching names, then an "ok <pkg> ..."
# line; pair every name with the package line that follows it.
mapfile -t targets < <(
  go test -list '^Fuzz' ${PKGS:-./...} 2>/dev/null |
    awk '/^Fuzz/ { t[n++] = $1 } /^ok[ \t]/ { for (i = 0; i < n; i++) print $2 " " t[i]; n = 0 }'
)

if [ "${#targets[@]}" -eq 0 ]; then
  echo "no Fuzz targets found" >&2
  exit 1
fi

echo "fuzzing ${#targets[@]} targets, ${fuzztime} each, -parallel ${parallel}"

# run_target fuzzes one target. Go's fuzzing engine sometimes ends a run with
# "--- FAIL ... context deadline exceeded" and no failing input: its own timer
# fired while a worker was still busy (the log shows execs falling to 0/sec at the
# end). That is the engine, not a bug in the code under test, and it showed up on
# roughly one target in ten in CI. A real finding always writes a failing input, so
# a run with no "Failing input written" line and that message is retried, up to
# three attempts; a real failure is never retried.
run_target() {
  local pkg="$1" name="$2" attempt out
  for attempt in 1 2 3; do
    out="$(mktemp)"
    if go test -run='^$' -fuzz="^${name}\$" -fuzztime="${fuzztime}" -parallel="${parallel}" "${pkg}" 2>&1 | tee "$out"; then
      rm -f "$out"
      return 0
    fi
    if grep -q 'Failing input written' "$out"; then
      rm -f "$out"
      return 1
    fi
    if grep -q 'context deadline exceeded' "$out" && [ "$attempt" -lt 3 ]; then
      echo "(the fuzz engine timed out without finding an input; retrying, attempt $((attempt + 1)) of 3)"
      rm -f "$out"
      continue
    fi
    rm -f "$out"
    return 1
  done
}

failed=()
for entry in "${targets[@]}"; do
  pkg="${entry%% *}"
  name="${entry##* }"
  echo "== ${pkg##*go-common/} ${name}"
  if ! run_target "${pkg}" "${name}"; then
    failed+=("${pkg##*go-common/}.${name}")
  fi
done

if [ "${#failed[@]}" -ne 0 ]; then
  echo "" >&2
  echo "FUZZ FAILURES: ${failed[*]}" >&2
  echo "Crashing inputs are under <pkg>/testdata/fuzz/<Target>/. Fix, then commit them." >&2
  exit 1
fi
echo "all ${#targets[@]} fuzz targets clean for ${fuzztime} each"
