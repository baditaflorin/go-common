#!/usr/bin/env bash
# Tests scripts/linkcheck.sh with a fake `go` on PATH (no toolchain, no network):
#   - a feature absent with its tag and present without it passes;
#   - a feature a tagged build still links FAILS, and names the importing package;
#   - a feature a default build no longer links FAILS (the tag removes nothing);
#   - a line with a tag and no package, and an empty budget, FAIL;
#   - the script pins the build target (the fake go refuses to run otherwise).
set -euo pipefail

root="$(cd "${BASH_SOURCE[0]%/*}/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/repo/scripts"
cp "$root/scripts/linkcheck.sh" "$tmp/repo/scripts/linkcheck.sh"
repo="$tmp/repo"

# The fake go answers `go list -deps [-tags T] -f FMT REF` from $FAKE/default (no tags) or
# $FAKE/tagged (a tag): lines of ImportPath|Imports.
cat >"$tmp/bin/go" <<'FAKE'
#!/usr/bin/env bash
if [ "${CGO_ENABLED:-}" != 0 ] || [ "${GOOS:-}" != linux ] || [ "${GOARCH:-}" != amd64 ]; then
  echo "fake go: the build target is not pinned to linux/amd64 without cgo" >&2
  exit 1
fi
[ "$1" = list ] || { echo "fake go: unexpected arguments: $*" >&2; exit 1; }
if [[ " $* " == *" -tags "* ]]; then cat "$FAKE/tagged"; else cat "$FAKE/default"; fi
FAKE
chmod +x "$tmp/bin/go"
export PATH="$tmp/bin:$PATH" FAKE="$tmp"

default_all() {
  printf '%s\n' 'example.com/svc|example.com/m/server' \
    'example.com/m/server|example.com/m/spiffe github.com/grafana/pyroscope-go' \
    'example.com/m/spiffe|github.com/spiffe/go-spiffe/v2/workloadapi' \
    'github.com/spiffe/go-spiffe/v2/workloadapi|github.com/go-jose/go-jose/v4' \
    'github.com/go-jose/go-jose/v4|' \
    'github.com/grafana/pyroscope-go|'
}
default_all >"$tmp/default"
printf '%s\n' 'example.com/svc|example.com/m/server' 'example.com/m/server|' >"$tmp/tagged"
mkdir -p "$repo/scripts"
cat >"$repo/scripts/linkcheck.txt" <<'B'
# the budget
nospiffe github.com/spiffe/go-spiffe/v2 github.com/go-jose/go-jose/v4
nopyroscope github.com/grafana/pyroscope-go
B

run() { (cd "$repo" && scripts/linkcheck.sh "$@") 2>&1; }
fail() { echo "linkcheck-selftest: FAIL: $*" >&2; exit 1; }

out="$(run)" || fail "a holding budget failed: $out"
[[ "$out" == *"2 opt-out tag(s) hold"* ]] || fail "no verdict line: $out"

# a tagged build that still links the feature
printf '%s\n' 'example.com/svc|example.com/m/server' 'example.com/m/server|github.com/grafana/pyroscope-go' \
  'github.com/grafana/pyroscope-go|' >"$tmp/tagged"
if out="$(run)"; then fail "a tagged build that links pyroscope passed"; fi
[[ "$out" == *"still links github.com/grafana/pyroscope-go"* && "$out" == *"example.com/m/server imports github.com/grafana/pyroscope-go"* ]] ||
  fail "the failure names neither the module nor the importer: $out"
printf '%s\n' 'example.com/svc|example.com/m/server' 'example.com/m/server|' >"$tmp/tagged"

# a default build that has lost the feature
printf '%s\n' 'example.com/svc|example.com/m/server' 'example.com/m/server|' >"$tmp/default"
if out="$(run)"; then fail "a default build without the feature passed"; fi
[[ "$out" == *"no longer links"* ]] || fail "no stale-line message: $out"
default_all >"$tmp/default"

# a prefix must match whole path elements: go-jose/go-jose/v4 is not go-jose/go-jose/v40
printf '%s\n' 'example.com/svc|example.com/m/server' 'example.com/m/server|github.com/go-jose/go-jose/v40' \
  'github.com/go-jose/go-jose/v40|' >"$tmp/tagged"
out="$(run)" || fail "a prefix matched half a path element: $out"
printf '%s\n' 'example.com/svc|example.com/m/server' 'example.com/m/server|' >"$tmp/tagged"

# a malformed budget
printf 'nospiffe\n' >"$repo/scripts/linkcheck.txt"
if out="$(run)"; then fail "a tag with no package passed"; fi
[[ "$out" == *"lists no package"* ]] || fail "no message for a tag with no package: $out"
printf '# nothing\n' >"$repo/scripts/linkcheck.txt"
if out="$(run)"; then fail "an empty budget passed"; fi
[[ "$out" == *"has no line"* ]] || fail "no message for an empty budget: $out"

echo "linkcheck-selftest: ok"
