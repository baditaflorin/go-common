#!/usr/bin/env bash
# Build + vet every Go module found in this repository. The current tree is a
# single root module; discovery remains automatic if a future nested module is
# added.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if ! command -v go >/dev/null 2>&1; then
  echo "error: go toolchain not found on PATH — cannot build modules." >&2
  exit 1
fi

fail=0
# -prune the vendor dirs; sort so the root module (shortest path) goes first.
while IFS= read -r gomod; do
  dir="$(dirname "$gomod")"
  rel="${dir#"$repo_root"}"; rel="${rel#/}"; [ -z "$rel" ] && rel="(root)"
  echo "== module: $rel =="
  if ! ( cd "$dir" && go build ./... && go vet ./... ); then
    echo "  FAIL: build/vet failed in $rel" >&2
    fail=1
  fi
done < <(find . -name go.mod -not -path '*/vendor/*' -print | sort)

if [ "$fail" -ne 0 ]; then
  echo "" >&2
  echo "ERROR: one or more modules failed to build/vet (see above)." >&2
  echo "       Run the failed module's build and vet commands locally." >&2
  exit 1
fi
echo "all modules build + vet clean"
