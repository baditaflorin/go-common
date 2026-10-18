#!/usr/bin/env bash
# Tests scripts/forward-compat.sh with a fake `go` on PATH, so it needs no toolchain
# and takes a moment. It checks that the script
#   - edits the go line of a COPY only (the checkout is byte-identical afterwards,
#     and the copy is left out of the history and is removed when it finishes);
#   - runs go vet and go test in that copy, with the go line it was asked for
#     (the default one when it is not asked), and GOTOOLCHAIN=local;
#   - exits non-zero when the tests fail, or vet fails, and names the packages
#     and tests, while still running the other of the two;
#   - exits 2, before running anything, for a toolchain older than the line, a
#     line that is not newer than go.mod's, and an argument that is not a minor.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin" "$tmp/scratch"

# The fake go. Every call is logged with where it ran and what it saw; vet and test
# behave as FAKE_VET and FAKE_TEST say.
cat >"$tmp/bin/go" <<'FAKE'
#!/usr/bin/env bash
saw() {
  printf '%s cwd=%s go=%s toolchain=%s git=%s\n' "$1" "$PWD" \
    "$(awk '$1 == "go" { print $2 }' go.mod)" "${GOTOOLCHAIN:-unset}" "$([ -e .git ] && echo present || echo absent)" >>"$FAKE_LOG"
}
case "$1" in
  env) echo "${FAKE_GOVERSION:-go1.27.0}" ;;
  mod)
    saw "mod $*"
    sed "s/^go .*/go ${3#-go=}/" go.mod >go.mod.new && mv go.mod.new go.mod ;;
  vet)
    saw vet
    if [ "${FAKE_VET:-pass}" = fail ]; then printf '# example.com/go-common/c\nc.go:3:1: boom\n'; exit 1; fi ;;
  test)
    saw test
    case "${FAKE_TEST:-pass}" in
      pass) printf 'ok  \texample.com/go-common/a\t0.001s\n' ;;
      fail)
        printf 'ok  \texample.com/go-common/a\t0.001s\n--- FAIL: TestBoom (0.00s)\n    --- FAIL: TestBoom/sub (0.00s)\nFAIL\nFAIL\texample.com/go-common/b\t0.002s\nFAIL\n'
        exit 1 ;;
      unnamed) printf 'go: updates to go.mod needed\n'; exit 1 ;;
    esac ;;
esac
FAKE
chmod +x "$tmp/bin/go"

# A checkout to run it in: the script under test, a go line, a .git that must not be copied.
repo="$tmp/repo"
mkdir -p "$repo/scripts" "$repo/.git" "$repo/sub"
cp "$root/scripts/forward-compat.sh" "$repo/scripts/forward-compat.sh"
printf 'FORWARD_COMPAT_GO_MINOR=1.26\n' >"$repo/scripts/tool-versions.sh"
printf 'module example.com/go-common\n\ngo 1.25.0\n' >"$repo/go.mod"
echo history >"$repo/.git/HEAD"
echo "uncommitted work" >"$repo/sub/file.txt"
snapshot() { (cd "$repo" && find . -type f -print0 | sort -z | xargs -0 cksum); }
before="$(snapshot)"

fail=0
complain() { echo "FAIL: $1" >&2; fail=1; }

# run_case <name> <want exit> [args]: runs the script, leaves $out and $log for the checks.
run_case() {
  local name="$1" want="$2" got=0
  shift 2
  : >"$tmp/log"
  # GOTOOLCHAIN=auto, the Go default: the script must override it, not inherit a `local` from the image.
  FAKE_LOG="$tmp/log" GOTOOLCHAIN=auto PATH="$tmp/bin:$PATH" TMPDIR="$tmp/scratch" "$repo/scripts/forward-compat.sh" "$@" >"$tmp/out" 2>&1 || got=$?
  out="$(cat "$tmp/out")"
  log="$(cat "$tmp/log")"
  if [ "$got" -ne "$want" ]; then
    complain "$name: exit=$got, want $want"
    sed 's/^/    /' "$tmp/out" >&2
  else
    echo "ok: $name (exit $got)"
  fi
}
expect() { # <name> <text it must contain> <where: out|log>
  case "${!3}" in *"$2"*) ;; *) complain "$1: no '$2' in the $3"; sed 's/^/    /' "$tmp/$3" >&2 ;; esac
}
refuse() { # <name> <text it must not contain> <where>
  case "${!3}" in *"$2"*) complain "$1: unexpected '$2' in the $3"; sed 's/^/    /' "$tmp/$3" >&2 ;; esac
}
unchanged() { # <name>
  [ "$(snapshot)" = "$before" ] || complain "$1: the checkout was changed"
  [ -z "$(ls "$tmp/scratch")" ] || complain "$1: the copy was not removed: $(ls "$tmp/scratch")"
}

# 1. Passing run with an explicit line.
run_case pass-explicit 0 1.27
expect pass-explicit "mod edit -go=1.27.0 cwd=" log
for step in vet test; do
  # each ran once, after the edit, in the copy
  case "$(grep "^$step " <<<"$log")" in
    *"go=1.27.0 toolchain=local git=absent") ;;
    *) complain "pass-explicit: $step did not run on go 1.27.0 with GOTOOLCHAIN=local and no .git"; sed 's/^/    /' "$tmp/log" >&2 ;;
  esac
done
refuse pass-explicit "cwd=$repo " log
expect pass-explicit "pass with go 1.27.0" out
unchanged pass-explicit

# 2. No argument: the line from tool-versions.sh.
FAKE_GOVERSION=go1.26.9 run_case pass-default 0
expect pass-default "vet cwd=" log
case "$(grep '^test ' <<<"$log")" in *"go=1.26.0 toolchain=local"*) ;; *) complain "pass-default: test did not run on go 1.26.0" ;; esac
unchanged pass-default

# 3. Failing tests: non-zero, named, and vet still ran.
FAKE_TEST=fail run_case test-fail 1 1.27
expect test-fail "tests failed in b: TestBoom" out
refuse test-fail "TestBoom/sub," out
refuse test-fail "tests failed in a" out
expect test-fail "vet cwd=" log
unchanged test-fail

# 4. Failing vet: non-zero, named, and the tests still ran.
FAKE_VET=fail run_case vet-fail 1 1.27
expect vet-fail "go vet failed (exit 1): c" out
expect vet-fail "test cwd=" log
unchanged vet-fail

# 5. A failure that names no package is still a failure.
FAKE_TEST=unnamed run_case test-unnamed 1 1.27
expect test-unnamed "without naming a package" out
unchanged test-unnamed

# 6. Nothing is run when it cannot test what was asked.
FAKE_GOVERSION=go1.26.9 run_case toolchain-too-old 2 1.27
expect toolchain-too-old "go1.26.9" out
[ -z "$log" ] || complain "toolchain-too-old: ran go: $log"
run_case not-newer 2 1.25
expect not-newer "not newer" out
[ -z "$log" ] || complain "not-newer: ran go: $log"
for bad in 1.27.0 latest 127; do
  run_case "bad-argument '$bad'" 2 "$bad"
  [ -z "$log" ] || complain "bad-argument '$bad': ran go: $log"
done
unchanged refusals

# 7. FORWARD_COMPAT_KEEP=1 keeps the copy, with the changed line, and still not the checkout.
FORWARD_COMPAT_KEEP=1 run_case keep 0 1.27
kept="$(ls -d "$tmp"/scratch/forward-compat.*/tree 2>/dev/null | head -1)"
if [ -z "$kept" ] || ! grep -qx 'go 1.27.0' "$kept/go.mod" || [ -e "$kept/.git" ]; then
  complain "keep: no copy with go 1.27.0 and no .git at ${kept:-<none>}"
fi
[ "$(snapshot)" = "$before" ] || complain "keep: the checkout was changed"

exit "$fail"
