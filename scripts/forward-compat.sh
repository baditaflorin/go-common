#!/usr/bin/env bash
# Runs go-common's own checks as if its `go` line were newer, so the behaviour
# changes Go makes for a newer line show up before the minimum Go moves.
#
#   scripts/forward-compat.sh [GO_MINOR]     # e.g. 1.27; default FORWARD_COMPAT_GO_MINOR
#   FORWARD_COMPAT_KEEP=1 scripts/forward-compat.sh 1.27   # keep the copy to debug in
#
# Why this exists: Go picks the default value of every GODEBUG setting from the
# `go` line of the module being built, not from the toolchain. go.mod says
# `go 1.26.0`, so the tests on a Go 1.27 toolchain still run with the 1.26
# defaults, and a setting Go flips for 1.27 (tracebacklabels, for one) cannot fail
# anything until a service raises its own go line. This
# script copies the working tree to a temporary directory, runs
# `go mod edit -go=<GO_MINOR>.0` there, then `go vet ./...` and
# `go test -count=1 ./...`, and names what fails. The working tree is never
# touched, and the changed go line is never committed.
#
# The toolchain that runs it must be at least GO_MINOR: GOTOOLCHAIN is forced to
# `local`, so it never downloads one, and it stops with exit 2 when the Go on PATH
# is older. Exit 0: vet and tests pass on the newer line. Exit 1: something fails
# (the summary says which packages and tests). Exit 2: it could not run.
#
# Only the root module is checked (./... does not enter lint/fixture, a module of
# its own). It runs go vet and go test, which are CPU-heavy: run it in CI or on a
# build host rather than a laptop.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=scripts/tool-versions.sh
source scripts/tool-versions.sh

minor="${1:-${FORWARD_COMPAT_GO_MINOR}}"
die() { echo "forward-compat: $*" >&2; exit 2; }

[[ "$minor" =~ ^([0-9]+)\.([0-9]+)$ ]] || die "the Go version must be a minor version such as 1.27, not '${minor}'"
want_major="${BASH_REMATCH[1]}"
want_minor="${BASH_REMATCH[2]}"

[ -f go.mod ] || die "no go.mod here; run it from a go-common checkout"
current="$(awk '$1 == "go" { print $2; exit }' go.mod)"
[[ "$current" =~ ^([0-9]+)\.([0-9]+) ]] || die "cannot read the go line of go.mod"
if [ "${BASH_REMATCH[1]}" -gt "$want_major" ] ||
  { [ "${BASH_REMATCH[1]}" -eq "$want_major" ] && [ "${BASH_REMATCH[2]}" -ge "$want_minor" ]; }; then
  die "go.mod already says go ${current}; ${minor} is not newer, so it would test nothing. Pass a newer minor (and raise FORWARD_COMPAT_GO_MINOR in scripts/tool-versions.sh)"
fi

# Never fetch a toolchain: the Go that runs is the Go that is tested.
export GOTOOLCHAIN=local
gover="$(go env GOVERSION)"
[[ "$gover" =~ go([0-9]+)\.([0-9]+) ]] || die "cannot read the Go version from '${gover}'"
if [ "${BASH_REMATCH[1]}" -lt "$want_major" ] ||
  { [ "${BASH_REMATCH[1]}" -eq "$want_major" ] && [ "${BASH_REMATCH[2]}" -lt "$want_minor" ]; }; then
  die "go ${minor} was asked for but this toolchain is ${gover}; run it with Go ${minor} or newer (an image such as golang:${minor})"
fi

tmp="$(mktemp -d "${TMPDIR:-/tmp}/forward-compat.XXXXXX")"
if [ "${FORWARD_COMPAT_KEEP:-}" = "1" ]; then
  echo "forward-compat: the copy is kept at ${tmp}/tree"
else
  trap 'rm -rf "$tmp"' EXIT
fi
mkdir "$tmp/tree"
# The working tree as it is (uncommitted edits included), without the history.
tar --exclude=./.git -cf - . | tar -xf - -C "$tmp/tree"
cd "$tmp/tree"
module="$(awk '$1 == "module" { print $2; exit }' go.mod)"

go mod edit -go="${minor}.0"
echo "forward-compat: go.mod says go ${current}; testing with go ${minor}.0 on ${gover}"

vet_rc=0
test_rc=0
echo "== go vet ./..."
go vet ./... 2>&1 | tee "$tmp/vet.log" || vet_rc="${PIPESTATUS[0]}"
echo "== go test -count=1 ./..."
go test -count=1 ./... 2>&1 | tee "$tmp/test.log" || test_rc="${PIPESTATUS[0]}"

if [ "$vet_rc" -eq 0 ] && [ "$test_rc" -eq 0 ]; then
  echo "forward-compat: go vet and go test pass with go ${minor}.0"
  exit 0
fi

echo
echo "forward-compat: FAIL with go ${minor}.0 (go.mod says ${current})"
if [ "$vet_rc" -ne 0 ]; then
  # go vet prints "# <package>" above each package's findings.
  vet_pkgs="$(sed -n 's/^# \([^ ]*\).*/\1/p' "$tmp/vet.log" | sort -u | sed "s|^${module}/||" | paste -sd ' ' -)"
  echo "  go vet failed (exit ${vet_rc}): ${vet_pkgs:-no package named, see the output above}"
fi
if [ "$test_rc" -ne 0 ]; then
  # A top-level "--- FAIL: TestX" precedes the "FAIL <package> <time>" line of its package.
  failed="$(awk -v mod="${module}/" '
    /^--- FAIL: / { tests = tests (tests == "" ? "" : ", ") $3; next }
    /^FAIL[ \t]+[^ \t]/ {
      pkg = $2; sub("^" mod, "", pkg)
      print "  tests failed in " pkg (tests == "" ? "" : ": " tests)
      tests = ""
    }' "$tmp/test.log")"
  echo "${failed:-  go test failed (exit ${test_rc}) without naming a package, see the output above}"
fi
echo "A newer go line changes GODEBUG defaults (https://go.dev/doc/godebug): fix the code first; see docs/releasing.md."
exit 1
