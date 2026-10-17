#!/usr/bin/env bash
# Tests scripts/dep-budget.sh against a small made-up module, with a fake `go` on
# PATH, so it needs no toolchain, no network and takes a moment:
#   - a budget that matches reality passes;
#   - a budget made stricter than reality FAILS, naming the package and the module;
#   - a small package that starts importing a heavier one FAILS, and the message says
#     which go-common package brings the module in and how to get there;
#   - a module reached only through another third-party module is reported as such;
#   - a budget that names a missing package, or includes an unknown @package, FAILS;
#   - a budget that allows more than is reached passes with a note to tighten it;
#   - --print, --write-doc and --check-doc work, and --check-doc FAILS on a stale page;
#   - the script pins the build target (the fake go refuses to run otherwise).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/repo/scripts" "$tmp/repo/docs"

# Run the script in a copy, so its `cd` lands in a repo of its own.
cp "$root/scripts/dep-budget.sh" "$tmp/repo/scripts/dep-budget.sh"
printf 'module example.com/m\n\ngo 1.25.0\n' >"$tmp/repo/go.mod"
repo="$tmp/repo"

# The fake go prints $FAKE_GRAPH for the one command the script runs.
cat >"$tmp/bin/go" <<'FAKE'
#!/usr/bin/env bash
case "$*" in
  "list -deps -f "*" ./...") ;;
  *) echo "fake go: unexpected arguments: $*" >&2; exit 1 ;;
esac
if [ "${CGO_ENABLED:-}" != 0 ] || [ "${GOOS:-}" != linux ] || [ "${GOARCH:-}" != amd64 ]; then
  echo "fake go: the build target is not pinned to linux/amd64 without cgo" >&2
  exit 1
fi
cat "$FAKE_GRAPH"
FAKE
chmod +x "$tmp/bin/go"

# graph writes the fake `go list -deps` output: ImportPath|Name|Module|Imports, each
# package after its dependencies. The first-party packages (example.com/m):
#   small        imports only the standard library
#   mid          imports a third-party package (module example.org/leaf), which imports
#                a package of example.org/deep
#   big          imports mid and example.org/heavy
#   deep/pkg     stdlib only
# plus ones that are not importable and must be ignored: internal/util,
# testhelpers/h, scripts/gen and tool (a main package).
# $1 is what `small` imports.
graph() {
  cat <<EOF
unsafe|unsafe||
strings|strings||unsafe
example.org/deep/y|y|example.org/deep|strings
example.org/leaf/x|x|example.org/leaf|example.org/deep/y strings
example.org/heavy|heavy|example.org/heavy|strings
example.com/m/internal/util|util|example.com/m|example.org/heavy
example.com/m/mid|mid|example.com/m|example.org/leaf/x strings
example.com/m/small|small|example.com/m|$1
example.com/m/big|big|example.com/m|example.com/m/mid example.org/heavy
example.com/m/deep/pkg|pkg|example.com/m|strings
example.com/m/testhelpers/h|h|example.com/m|example.org/heavy
example.com/m/scripts/gen|main|example.com/m|example.org/heavy
example.com/m/tool|main|example.com/m|example.org/heavy
EOF
}

budget() { cat >"$repo/scripts/dep-budget.txt"; }
good_budget() {
  budget <<'EOF'
# The budget for the made-up module.
small
mid example.org/leaf example.org/deep   # leaf, and what leaf brings in
big @mid
big example.org/heavy
deep/pkg
EOF
}

fail=0
name=""
# t NAME WANT-EXIT [ARG...] runs the script; its output is left in $tmp/out.
t() {
  name="$1"; local want="$2" got=0
  shift 2
  # The environment is hostile on purpose: the script has to pin the build target itself.
  (cd "$repo" && FAKE_GRAPH="$tmp/graph" PATH="$tmp/bin:$PATH" CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 scripts/dep-budget.sh "$@") >"$tmp/out" 2>&1 || got=$?
  if [ "$got" -ne "$want" ]; then
    echo "FAIL: $name: exit=$got, want $want" >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=1
    return 1
  fi
  echo "ok: $name (exit $got)"
}
has() {
  grep -qF -- "$1" "$tmp/out" || { echo "FAIL: $name: output does not contain: $1" >&2; sed 's/^/    /' "$tmp/out" >&2; fail=1; }
}
hasnt() {
  if grep -qF -- "$1" "$tmp/out"; then echo "FAIL: $name: output should not contain: $1" >&2; sed 's/^/    /' "$tmp/out" >&2; fail=1; fi
}

graph strings >"$tmp/graph"

good_budget
t "a budget that matches reality passes" 0 || true
has "ok: 4 budgeted packages are within budget (4 importable packages measured)"
hasnt "no longer reaches"

# Stricter than reality: big may no longer reach example.org/heavy.
budget <<'EOF'
small
mid example.org/leaf example.org/deep
big @mid
deep/pkg
EOF
t "a budget stricter than reality fails" 1 || true
has "big: 1 new third-party module"
has "example.org/heavy"
has "imported by big itself"
has "scripts/dep-budget.txt"        # it says where to go
has "move the code"                 # and what to do about it
hasnt "    small:"
hasnt "    mid:"

# A stdlib-only package that starts to import mid: it gains leaf (imported by mid)
# and deep (reached only through leaf).
good_budget
graph "strings example.com/m/mid" >"$tmp/graph"
t "a small package that imports a heavier one fails" 1 || true
has "small: 2 new third-party modules"
has "example.org/leaf"
has "imported by mid (small -> mid)"
has "reached only through other third-party modules"
has "example.org/deep"
hasnt "    big:"
graph strings >"$tmp/graph"

# Slack: allowing more than is reached passes, with a note.
budget <<'EOF'
small example.org/deep
mid example.org/leaf example.org/deep
big @mid
big example.org/heavy
deep/pkg
EOF
t "a budget that allows more than is reached passes" 0 || true
has "small: no longer reaches example.org/deep"
hasnt "mid: no longer"

# Rot: a renamed or removed package must not silently drop out of the budget.
good_budget
printf 'ghost\n' >>"$repo/scripts/dep-budget.txt"
t "a budget that names a missing package fails" 1 || true
has "names ghost"
good_budget
printf 'internal/util\n' >>"$repo/scripts/dep-budget.txt"
t "a budget that names a non-importable package fails" 1 || true
has "names internal/util"
good_budget
printf 'small @nowhere\n' >>"$repo/scripts/dep-budget.txt"
t "a budget that includes an unknown package fails" 1 || true
has "includes @nowhere"
printf '# nothing\n' | budget
t "an empty budget fails" 1 || true
has "lists no packages"

# --print shows every importable package's set in the budget's syntax.
good_budget
t "--print" 0 --print || true
cat >"$tmp/want-print" <<'EOF'
big example.org/deep
big example.org/heavy
big example.org/leaf
deep/pkg
mid example.org/deep
mid example.org/leaf
small
EOF
if ! diff -u "$tmp/want-print" "$tmp/out" >"$tmp/diff"; then
  echo "FAIL: --print: unexpected output" >&2; sed 's/^/    /' "$tmp/diff" >&2; fail=1
fi

# The generated page: write, check, then let it go stale two ways.
t "--check-doc fails while the page does not exist" 1 --check-doc || true
has "stale"
t "--write-doc" 0 --write-doc || true
t "--check-doc passes on a fresh page" 0 --check-doc || true
grep -qF '| [`big`](../big/) | 3 | yes | `(itself)`, `mid` | `example.org/deep`, `example.org/heavy`, `example.org/leaf` |' "$repo/docs/dependencies.md" || { echo "FAIL: the page has no row for big" >&2; sed 's/^/    /' "$repo/docs/dependencies.md" >&2; fail=1; }
grep -qF '| [`small`](../small/) | 0 | yes | - | - |' "$repo/docs/dependencies.md" || { echo "FAIL: the page has no row for small" >&2; fail=1; }
if grep -qF '[`internal/util`]' "$repo/docs/dependencies.md" || grep -qE 'testhelpers/h|scripts/gen|`tool`' "$repo/docs/dependencies.md"; then
  echo "FAIL: the page lists a package that is not importable" >&2; fail=1
fi
graph "strings example.com/m/mid" >"$tmp/graph"
t "--check-doc fails when a package's set changed" 1 --check-doc || true
has "stale"
graph strings >"$tmp/graph"
printf 'a hand edit\n' >>"$repo/docs/dependencies.md"
t "--check-doc fails on a hand-edited page" 1 --check-doc || true
t "--write-doc repairs it" 0 --write-doc || true
t "--check-doc passes again" 0 --check-doc || true

exit "$fail"
