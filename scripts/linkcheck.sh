#!/usr/bin/env bash
# Link budget: what a server.New service links, with and without the opt-out build tags.
#
#   scripts/linkcheck.sh           check scripts/linkcheck.txt (CI runs this)
#   scripts/linkcheck.sh --size    check, then build the reference service with and without
#                                  every tag and print the stripped sizes (not a gate)
#
# go-common is one module and a service links every package its imports reach, so a
# feature that few services use (the SPIFFE Workload API client, the Pyroscope client) is
# linked into all of them unless something keeps it out. The opt-out build tags
# (gocommon_nospiffe, gocommon_nopyroscope) keep it out of a binary built with them and
# change nothing for a binary built without. This script holds both halves: with the tag
# the feature's packages are absent from `go list -deps` of scripts/linkcheck/service, and
# without it they are present, so a refactor cannot quietly make a tag a no-op or make a
# default build lose a feature. `go list` only reads imports: no linking, about a second.
#
# Needs bash, awk and go. scripts/linkcheck-selftest.sh tests this script with a stub `go`.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

budget="${LINKCHECK_BUDGET:-scripts/linkcheck.txt}"
ref="${LINKCHECK_REF:-./scripts/linkcheck/service}"
size=0
case "${1:-}" in
  "") ;;
  --size) size=1 ;;
  -h | --help) sed -n '2,6p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
  *) echo "linkcheck: unknown argument: $1" >&2; exit 2 ;;
esac
[ -f "$budget" ] || { echo "linkcheck: $budget not found" >&2; exit 1; }

# The fleet's build target, so the answer is the same on a laptop and in CI.
export LC_ALL=C CGO_ENABLED=0 GOOS=linux GOARCH=amd64

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# deps TAGS FILE writes one line per package the reference service links: ImportPath|Imports.
deps() {
  local tags="$1" out="$2" args=(-deps)
  [ -n "$tags" ] && args+=(-tags "$tags")
  if ! go list "${args[@]}" -f '{{.ImportPath}}|{{join .Imports " "}}' "$ref" >"$out" 2>"$tmp/err"; then
    cat "$tmp/err" >&2
    echo "linkcheck: 'go list -deps${tags:+ -tags $tags} $ref' failed" >&2
    exit 1
  fi
}

# has FILE PREFIX: is a package under PREFIX in the list?
has() { awk -F'|' -v p="$2" '$1 == p || index($1, p "/") == 1 { found = 1 } END { exit !found }' "$1"; }

# importers FILE PREFIX: the packages of the list that import something under PREFIX.
importers() {
  awk -F'|' -v p="$2" '{
    n = split($2, imp, " ")
    for (i = 1; i <= n; i++) if (imp[i] == p || index(imp[i], p "/") == 1) { print "    " $1 " imports " imp[i]; break }
  }' "$1" | head -n 5 || true
}

deps "" "$tmp/default"
bad=0
lines=0
alltags=()
while IFS= read -r line; do
  line="${line%%#*}"
  # shellcheck disable=SC2086
  set -- $line
  [ "$#" -eq 0 ] && continue
  tags="$1"; shift
  if [ "$#" -eq 0 ]; then
    echo "linkcheck: $budget: '$tags' lists no package" >&2
    bad=1
    continue
  fi
  lines=$((lines + 1))
  alltags+=("$tags")
  deps "$tags" "$tmp/tagged"
  for p in "$@"; do
    if ! has "$tmp/default" "$p"; then
      echo "linkcheck: a default build of $ref no longer links $p, so -tags $tags removes nothing:" >&2
      echo "  the line is stale, or the default stack lost the feature; fix scripts/linkcheck.txt on purpose" >&2
      bad=1
    fi
    if has "$tmp/tagged" "$p"; then
      echo "linkcheck: a build with -tags $tags still links $p:" >&2
      importers "$tmp/tagged" "$p" >&2
      echo "  put the import behind the tag (a file with //go:build !$tags and one with //go:build $tags)" >&2
      bad=1
    fi
  done
done <"$budget"
if [ "$lines" -eq 0 ]; then
  echo "linkcheck: $budget has no line" >&2
  exit 1
fi
[ "$bad" -eq 0 ] || exit 1
echo "linkcheck: $lines opt-out tag(s) hold: absent with the tag, present without it"

if [ "$size" -eq 1 ]; then
  all="$(IFS=,; echo "${alltags[*]}")"
  go build -trimpath -ldflags='-s -w' -o "$tmp/default.bin" "$ref"
  go build -trimpath -ldflags='-s -w' -tags "$all" -o "$tmp/tagged.bin" "$ref"
  d="$(wc -c <"$tmp/default.bin" | tr -d ' ')"
  t="$(wc -c <"$tmp/tagged.bin" | tr -d ' ')"
  echo "linkcheck: reference service, stripped: default ${d} bytes; -tags ${all} ${t} bytes (-$((d - t)))"
fi
