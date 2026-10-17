#!/usr/bin/env bash
# Tests scripts/fuzz.sh's handling of the Go fuzz engine's spurious timeout, with a
# fake `go` on PATH, so it needs no toolchain and takes a moment:
#   - a bare "context deadline exceeded" is retried and then passes;
#   - a failure that wrote a failing input is reported at once, not retried;
#   - a timeout that never clears is eventually reported.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"

# The fake go: `test -list` names one target; `test -fuzz` behaves as FAKE_MODE says.
cat >"$tmp/bin/go" <<'FAKE'
#!/usr/bin/env bash
case " $* " in
  *" -list "*)
    printf 'FuzzFake\nok  \texample.com/go-common/fake\t0.001s\n'
    exit 0 ;;
esac
n=$(( $(cat "$FAKE_COUNT" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$FAKE_COUNT"
case "$FAKE_MODE" in
  flaky-once)
    if [ "$n" -eq 1 ]; then printf -- '--- FAIL: FuzzFake (6.01s)\n    context deadline exceeded\nFAIL\n'; exit 1; fi
    printf 'PASS\n'; exit 0 ;;
  real-finding)
    printf -- '--- FAIL: FuzzFake (0.4s)\n    boom\n    Failing input written to testdata/fuzz/FuzzFake/abc\nFAIL\n'; exit 1 ;;
  always-timeout)
    printf -- '--- FAIL: FuzzFake (6.01s)\n    context deadline exceeded\nFAIL\n'; exit 1 ;;
esac
FAKE
chmod +x "$tmp/bin/go"

# Run fuzz.sh in a copy so its `cd` lands next to nothing important.
mkdir -p "$tmp/repo/scripts"
cp "$root/scripts/fuzz.sh" "$tmp/repo/scripts/fuzz.sh"

fail=0
run() { # mode expected-exit expected-attempts
  local mode="$1" want_exit="$2" want_attempts="$3" exit_code=0
  : >"$tmp/count"
  FAKE_MODE="$mode" FAKE_COUNT="$tmp/count" PATH="$tmp/bin:$PATH" "$tmp/repo/scripts/fuzz.sh" 1s >"$tmp/out.log" 2>&1 || exit_code=$?
  local attempts
  attempts="$(cat "$tmp/count")"
  if [ "$exit_code" -ne "$want_exit" ] || [ "$attempts" -ne "$want_attempts" ]; then
    echo "FAIL: $mode: exit=$exit_code attempts=$attempts, want exit=$want_exit attempts=$want_attempts" >&2
    sed 's/^/    /' "$tmp/out.log" >&2
    fail=1
  else
    echo "ok: $mode (exit $exit_code after $attempts attempt(s))"
  fi
}

run flaky-once 0 2      # the engine's timer fired; the retry passes
run real-finding 1 1    # a real failing input: reported at once, never retried
run always-timeout 1 3  # gives up after three attempts

exit "$fail"
