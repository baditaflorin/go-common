#!/usr/bin/env bash
# Checks that the release a change claims is the next free one.
#
#   scripts/release-check.sh [base-ref]
#
# A change that adds a "## [X.Y.Z] - YYYY-MM-DD" entry to CHANGELOG.md claims
# version X.Y.Z. This fails when
#   - it adds more than one version entry (one change, one release),
#   - the heading is not in that exact form,
#   - the tag vX.Y.Z already exists: another change released it first. Two changes
#     both claimed 0.102.66 on 2026-10-17, and the same happened to 0.102.67 and
#     0.102.68; whichever merges second must renumber,
#   - X.Y.Z is not newer than the newest tag, or than the previous top entry.
# A change that leaves CHANGELOG.md alone claims no release, and passes.
#
# Tags are fetched first (a CI clone often has none). The base comes from
# scripts/lib/base-ref.sh. Nothing here creates a tag: this repository's CI does
# not tag, see docs/releasing.md.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=scripts/lib/base-ref.sh
source scripts/lib/base-ref.sh

git fetch -q --tags origin 2>/dev/null || echo "release-check: warning: could not fetch tags; checking against the local ones" >&2
if ! base="$(resolve_base_commit "${1:-}")"; then
  echo "release-check: could not determine the base to compare with" >&2
  exit 1
fi

added="$(git diff "$base" -- CHANGELOG.md | grep -E '^\+## \[' | sed 's/^+//' || true)"
if [ -z "$added" ]; then
  echo "release-check: CHANGELOG.md gains no version entry, so this change claims no release"
  exit 0
fi

fail() { echo "release-check: FAIL: $*" >&2; exit 1; }

if [ "$(printf '%s\n' "$added" | wc -l)" -ne 1 ]; then
  fail "this change adds more than one version entry to CHANGELOG.md:
$added"
fi
if ! printf '%s\n' "$added" | grep -Eq '^## \[[0-9]+\.[0-9]+\.[0-9]+\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$'; then
  fail "the new heading '$added' must look like '## [0.102.67] - 2026-10-17'"
fi
version="$(printf '%s\n' "$added" | sed -E 's/^## \[([0-9.]+)\].*/\1/')"

if git rev-parse -q --verify "refs/tags/v${version}" >/dev/null 2>&1; then
  fail "v${version} is already released ($(git rev-parse --short "v${version}^{commit}")). Another change took this version; rebase, then renumber your CHANGELOG.md entry to the next free one"
fi

newer() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" = "$1" ]; }

latest_tag="$(git tag --list 'v[0-9]*' --sort=-v:refname | head -1)"
if [ -n "$latest_tag" ] && ! newer "$version" "${latest_tag#v}"; then
  fail "${version} is not newer than the latest tag ${latest_tag}; use the next free version"
fi

prev_top="$(git show "${base}:CHANGELOG.md" 2>/dev/null | sed -n 's/^## \[\([0-9][0-9.]*\)\] - .*/\1/p' | head -1 || true)"
if [ -n "$prev_top" ] && ! newer "$version" "$prev_top"; then
  fail "${version} is not newer than the entry it is added above (${prev_top})"
fi

echo "release-check: ${version} is free (latest tag ${latest_tag:-none}, previous entry ${prev_top:-none})"
