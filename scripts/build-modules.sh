#!/usr/bin/env bash
# Build + vet EVERY Go module in this repo, including nested modules.
#
# Why this exists: `telemetry/` is a nested module (its own go.mod) so the
# heavy OpenTelemetry/gRPC/protobuf tree stays out of the root module's
# dependency graph (see CHANGELOG v0.62.0). The catch is that `go build
# ./...` / `go test ./...` / `fleet-runner build-test` at the repo root
# DO NOT descend into a subdirectory that has its own go.mod — Go treats it
# as a separate module. So a breaking change to a root package that the
# nested module imports (e.g. graph/promx/safehttp, which telemetry.go uses)
# would leave the root gate green while silently breaking telemetry, unseen
# until some future consumer pulls go-common/telemetry.
#
# This script closes that gap: it finds every go.mod and builds/vets each
# module on its own, so the nested module is a first-class part of the gate.
# New nested modules are picked up automatically — no edit needed here.
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
  echo "       Nested modules (e.g. telemetry/) are NOT covered by a root" >&2
  echo "       'go build ./...' — this script is what catches them." >&2
  exit 1
fi
echo "all modules build + vet clean"
