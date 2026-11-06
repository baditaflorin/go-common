#!/usr/bin/env bash
# Exercise the gate with fake benchmark output: no Go toolchain or network.
set -euo pipefail

root="$(cd "${BASH_SOURCE[0]%/*}/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/repo/scripts/lib"
cp "$root/scripts/bench-gate.sh" "$tmp/repo/scripts/bench-gate.sh"
cp "$root/scripts/lib/base-ref.sh" "$tmp/repo/scripts/lib/base-ref.sh"
git -C "$tmp/repo" init -q
git -C "$tmp/repo" add .
git -C "$tmp/repo" -c user.name=selftest -c user.email=selftest@example.invalid commit -qm fixture

cat >"$tmp/bin/go" <<'FAKE'
#!/usr/bin/env bash
case "$PWD" in
  */base) cat "$FAKE/base.out"; exit "${FAKE_BASE_RC:-0}" ;;
  *) cat "$FAKE/head.out"; exit "${FAKE_HEAD_RC:-0}" ;;
esac
FAKE
chmod +x "$tmp/bin/go" "$tmp/repo/scripts/bench-gate.sh"
export PATH="$tmp/bin:$PATH" FAKE="$tmp"

output() { # file allocs bytes time
  printf 'pkg: example.com/go-common/hot\nBenchmarkHot-8 2000 %s ns/op %s B/op %s allocs/op\n' "$4" "$3" "$2" >"$1"
}
run() { (cd "$tmp/repo" && scripts/bench-gate.sh HEAD) 2>&1; }
fail() { echo "bench-gate-selftest: FAIL: $*" >&2; exit 1; }
passes() { local out; out="$(run)" || fail "$1: $out"; }
rejects() {
  local out
  if out="$(run)"; then fail "$1 passed: $out"; fi
  [[ "$out" == *"$2"* ]] || fail "$1 had no expected verdict '$2': $out"
}

output "$tmp/base.out" 10 1000 100
output "$tmp/head.out" 10 1000 1000000
passes "CPU timing is advisory"
output "$tmp/head.out" 11 1100 100
passes "exactly ten percent is tolerated"
output "$tmp/head.out" 12 1000 100
rejects "allocation count growth" REGRESSION
output "$tmp/head.out" 10 1101 100
rejects "byte growth without more allocations" REGRESSION

output "$tmp/base.out" 0 0 100
output "$tmp/head.out" 1 63 100
passes "absolute tolerance absorbs small noise"
output "$tmp/head.out" 1 64 100
rejects "absolute byte boundary" REGRESSION
output "$tmp/base.out" 2 640 100
output "$tmp/head.out" 2 704 100
passes "relative byte boundary"
output "$tmp/head.out" 2 705 100
rejects "both byte thresholds exceeded" REGRESSION

output "$tmp/base.out" 10 1000 100
output "$tmp/head.out" 10 1000 100
printf 'BenchmarkExtra-8 2000 100 ns/op 100 B/op 1 allocs/op\n' >>"$tmp/head.out"
passes "a new benchmark is allowed"
printf 'BenchmarkRequired-8 2000 100 ns/op 100 B/op 1 allocs/op\n' >>"$tmp/base.out"
rejects "a required benchmark was removed" MISSING
: >"$tmp/head.out"
rejects "all benchmarks were removed" 'no benchmarks found'
output "$tmp/head.out" 10 1000 100
export FAKE_BASE_RC=1
rejects "base benchmark execution failed" 'benchmarks failed to run'
export FAKE_BASE_RC=0 FAKE_HEAD_RC=1
rejects "head benchmark execution failed" 'benchmarks failed to run'

echo "bench-gate-selftest: ok"
