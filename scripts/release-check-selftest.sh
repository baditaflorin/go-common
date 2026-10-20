#!/usr/bin/env bash
# Tests scripts/release-check.sh in throwaway git repositories (real git and real tags,
# no network), with the Go program it runs, scripts/whatchanged, copied in. It checks that
# the script
#   - passes when CHANGELOG.md gains no entry, or one for the next free version, and
#     fails for a version that is tagged, older than the newest tag, older than the entry
#     it is added above, added twice, or written in a heading that does not look right;
#   - fails for a **BREAKING...** bullet that has no summary: line, a summary or off-switch
#     of more than 200 characters (counted as characters, in any locale), a field line
#     that is not written exactly right, a marker that is not well formed, or a
#     compat/changes.json that is not what CHANGELOG.md says, whichever entry the problem
#     is in;
#   - names the line and the rule, and exits 0 for the same change written correctly.
# scripts/whatchanged has its own unit tests; this is the part that is about git, tags,
# exit codes and the order of the checks.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if ! command -v go >/dev/null 2>&1; then
  echo "release-check-selftest: go is needed: release-check.sh runs scripts/whatchanged" >&2
  exit 2
fi

# Nothing from the caller's git configuration may change what the throwaway repositories do.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GOTOOLCHAIN=local GOFLAGS=-buildvcs=false
g() { git -c user.name=selftest -c user.email=selftest@example.invalid -c commit.gpgsign=false -c tag.gpgsign=false "$@"; }

# The repository: the script under test, the program it runs and what that imports.
repo="$tmp/repo"
mkdir -p "$repo/scripts/lib" "$repo/scripts/whatchanged" "$repo/compat"
cp "$root/scripts/release-check.sh" "$repo/scripts/"
cp "$root/scripts/lib/base-ref.sh" "$repo/scripts/lib/"
for f in "$root"/scripts/whatchanged/*.go "$root"/compat/*.go; do
  case "$f" in *_test.go) ;; *) cp "$f" "$repo/${f#"$root"/}" ;; esac
done
cp "$root/compat/compat.json" "$root/compat/changes.json" "$repo/compat/"
printf 'module github.com/baditaflorin/go-common\n\n%s\n' "$(grep -m1 '^go ' "$root/go.mod")" >"$repo/go.mod"
cd "$repo"

cat >CHANGELOG.md <<'EOF'
# Changelog

## [0.105.0] - 2026-10-19

### Changed

- **BREAKING (behavioural):** a change that is already released.
  - summary: a change that is already released.
  - off-switch: `OLD=off`
- an ordinary bullet

## [0.104.0] - 2026-10-18

### Added

- an older entry
EOF
go run ./scripts/whatchanged -gen >/dev/null
g init -q
g add -A
g commit -q -m base
base="$(g rev-parse HEAD)"
g tag v0.105.0

# A second base, whose top entry is newer than the newest tag: a release that merged and is not tagged yet.
{ printf '# Changelog\n\n## [0.105.3] - 2026-10-20\n\n### Fixed\n\n- an untagged release\n\n'; tail -n +3 CHANGELOG.md; } >CHANGELOG.new
mv CHANGELOG.new CHANGELOG.md
go run ./scripts/whatchanged -gen >/dev/null
g commit -q -am "an untagged release"
ahead="$(g rev-parse HEAD)"

fail=0
complain() { echo "FAIL: $1" >&2; fail=1; }

# at <commit>: a clean checkout of it.
at() { g checkout -q -f "$1"; g clean -fdq; }
# add_entry <text>: put a new entry at the top of CHANGELOG.md, below the title.
add_entry() { { printf '# Changelog\n\n%s\n\n' "$1"; tail -n +3 CHANGELOG.md; } >CHANGELOG.new && mv CHANGELOG.new CHANGELOG.md; }
regen() { go run ./scripts/whatchanged -gen >/dev/null; }
chars() { awk -v n="$1" -v c="$2" 'BEGIN { for (i = 0; i < n; i++) printf "%s", c }'; }

# run_case <name> <want exit> [base]: runs the script on the working tree, leaves $out.
run_case() {
  local name="$1" want="$2" got=0
  LC_ALL=C ./scripts/release-check.sh "${3:-$base}" >"$tmp/out" 2>&1 || got=$?
  out="$(cat "$tmp/out")"
  if [ "$got" -ne "$want" ]; then
    complain "$name: exit=$got, want $want"
    sed 's/^/    /' "$tmp/out" >&2
  else
    echo "ok: $name (exit $got)"
  fi
}
expect() { # <name> <text the output must contain>
  case "$out" in *"$2"*) ;; *) complain "$1: no '$2' in the output"; sed 's/^/    /' "$tmp/out" >&2 ;; esac
}
# entry <version> <bullets...>: a new entry for the version, with the lines given as its bullets.
entry() { local v="$1"; shift; add_entry "$(printf '## [%s] - 2026-10-20\n\n### Changed\n\n' "$v"; printf '%s\n' "$@")"; }

# 1. What the script has always checked.
at "$base"
run_case untouched 0
expect untouched "claims no release"

at "$base"; entry 0.105.1 "- a plain bullet"; regen
run_case next-free-version 0
expect next-free-version "0.105.1 is free"

at "$base"; entry 0.105.0 "- a plain bullet"
run_case already-tagged 1
expect already-tagged "v0.105.0 is already released"

at "$base"; entry 0.104.5 "- a plain bullet"
run_case older-than-the-newest-tag 1
expect older-than-the-newest-tag "not newer than the latest tag v0.105.0"

at "$base"; entry 0.105.2 "- a plain bullet"; entry 0.105.1 "- another"
run_case two-entries 1
expect two-entries "more than one version entry"

at "$base"; add_entry "## [0.105.1]"
run_case heading-without-a-date 1
expect heading-without-a-date "must look like"

at "$ahead"; entry 0.105.2 "- a plain bullet"
run_case older-than-the-entry-below 1 "$ahead"
expect older-than-the-entry-below "not newer than the entry it is added above (0.105.3)"

# 2. The feed bullets: the fields, and the file generated from them.
at "$base"
entry 0.106.0 "- **BREAKING (behavioural):** a thing changes, at length." "  - summary: a thing changes." '  - off-switch: `X=off`'; regen
run_case bullet-with-its-fields 0
expect bullet-with-its-fields "v0.106.0: 1 feed bullets"
expect bullet-with-its-fields "0.106.0 is free"

at "$base"
entry 0.106.0 "- **BREAKING:** the API changes." "  - summary: the API changes."; regen
run_case bullet-without-an-off-switch 0

at "$base"
entry 0.106.0 "- **BREAKING (behavioural):** a thing changes."
run_case bullet-without-a-summary 1
expect bullet-without-a-summary "CHANGELOG.md:7: BREAKING (behavioural) of v0.106.0: no summary: line"
expect bullet-without-a-summary "release-check: FAIL"

at "$base"
entry 0.106.0 "- **BREAKING:** x" "  - summary: $(chars 200 a)"; regen
run_case summary-of-200-characters 0

at "$base"
entry 0.106.0 "- **BREAKING:** x" "  - summary: $(chars 201 a)"
run_case summary-of-201-characters 1
expect summary-of-201-characters "summary: is 201 characters; the limit is 200"

# 200 characters that take 400 bytes: the limit is in characters, whatever the locale (LC_ALL=C here).
at "$base"
entry 0.106.0 "- **BREAKING:** x" "  - summary: $(chars 200 é)"; regen
run_case summary-of-200-multibyte-characters 0
at "$base"
entry 0.106.0 "- **BREAKING:** x" "  - summary: $(chars 201 é)"
run_case summary-of-201-multibyte-characters 1

at "$base"
entry 0.106.0 "- **BREAKING:** x" "  - summary: s" "  - off-switch: $(chars 201 a)"
run_case off-switch-of-201-characters 1
expect off-switch-of-201-characters "off-switch: is 201 characters"

at "$base"
entry 0.106.0 "- **BREAKING:** x" "   - summary: three spaces"
run_case field-line-with-three-spaces 1
expect field-line-with-three-spaces "looks like a field"

at "$base"
entry 0.106.0 "- **BREAKING:** x" "  - summary: one" "  - summary: two"
run_case summary-given-twice 1
expect summary-given-twice "summary: is given twice"

at "$base"
entry 0.106.0 "- **BREAKING**: the colon is outside the bold" "  - summary: s"
run_case marker-not-well-formed 1
expect marker-not-well-formed "starts a BREAKING bullet but not as"

at "$base"
entry 0.106.0 "- **BREAKING (behavioural):** x" "  - summary: s"
run_case feed-not-regenerated 1
expect feed-not-regenerated "compat/changes.json is stale"

# The problem need not be in the new entry: the feed is generated from the whole file.
at "$base"
entry 0.105.1 "- a plain bullet"; regen
sed 's/^  - summary: a change that is already released\./  - summary: reworded./' CHANGELOG.md >CHANGELOG.new && mv CHANGELOG.new CHANGELOG.md
run_case an-old-summary-edited-without-regenerating 1
expect an-old-summary-edited-without-regenerating "compat/changes.json is stale"

at "$base"
entry 0.105.1 "- a plain bullet"
grep -v '^  - summary: a change that is already released\.$' CHANGELOG.md >CHANGELOG.new && mv CHANGELOG.new CHANGELOG.md
run_case an-old-bullet-without-a-summary 1
expect an-old-bullet-without-a-summary "of v0.105.0: no summary: line"

# 3. No go, no check: it must not pass silently.
at "$base"; entry 0.105.1 "- a plain bullet"; regen
mkdir "$tmp/nogo"
for tool in bash git grep sed sort head tail wc tr cat dirname mktemp rm awk env; do ln -s "$(command -v "$tool")" "$tmp/nogo/$tool"; done
got=0
PATH="$tmp/nogo" "$(command -v bash)" ./scripts/release-check.sh "$base" >"$tmp/out" 2>&1 || got=$?
out="$(cat "$tmp/out")"
if [ "$got" -ne 1 ]; then complain "no-go: exit=$got, want 1"; sed 's/^/    /' "$tmp/out" >&2; else echo "ok: no-go (exit 1)"; fi
expect no-go "go is needed"

exit "$fail"
