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
#   - fails a **BREAKING (behavioural):** bullet in a patch release (the same major.minor as
#     the newest tag, which is not the previous entry), and passes it in a minor or major
#     release, in a patch when the bullet is **BEHAVIOURAL (patch-safe):** with a reason:
#     line, and when there is no tag; and does not apply the rule to a **BREAKING:** bullet;
#   - fails a **SECURITY:** bullet unless compat/compat.json, in the same change, either raises
#     min_safe to this release (with the fix in reasons and the date moved) or records
#     min_safe_unchanged for it with a reason; one decision covers all the bullets of an entry;
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
expect marker-not-well-formed "bullet but not as"

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

# 3. A behaviour change takes a minor release, unless its bullet says it is safe in a patch, and why.
b='- **BREAKING (behavioural):** a thing changes.'
at "$base"; entry 0.105.1 "$b" "  - summary: a thing changes."; regen
run_case behavioural-change-in-a-patch 1
expect behavioural-change-in-a-patch "CHANGELOG.md:7: BREAKING (behavioural) of v0.105.1: a behaviour change takes a minor release"
expect behavioural-change-in-a-patch "follows v0.105.0 in the same minor"
at "$base"; entry 0.106.0 "$b" "  - summary: a thing changes."; regen
run_case behavioural-change-in-a-minor 0
at "$base"; entry 1.0.0 "$b" "  - summary: a thing changes."; regen
run_case behavioural-change-in-a-major 0
at "$ahead"; entry 0.105.4 "$b" "  - summary: a thing changes."; regen
run_case behavioural-change-in-a-patch-after-an-untagged-release 1 "$ahead"
expect behavioural-change-in-a-patch-after-an-untagged-release "follows v0.105.0 in the same minor"
at "$base"; entry 0.105.1 "$b" "  - summary: a thing changes." "$b" "  - summary: and another."; regen
run_case two-behavioural-changes-in-a-patch 1
expect two-behavioural-changes-in-a-patch "CHANGELOG.md:9: BREAKING (behavioural)"

at "$base"; entry 0.105.1 "- **BEHAVIOURAL (patch-safe):** only a call that failed before is refused." "  - summary: calls that failed before are refused." "  - reason: no call that worked changes."; regen
run_case patch-safe-with-a-reason 0
at "$base"; entry 0.105.1 "- **BEHAVIOURAL (patch-safe):** x" "  - summary: s"
run_case patch-safe-without-a-reason 1
expect patch-safe-without-a-reason "no reason: line"
at "$base"; entry 0.105.1 "- **BEHAVIOURAL (urgent):** x" "  - summary: s" "  - reason: r"
run_case behavioural-with-another-qualifier 1
expect behavioural-with-another-qualifier "BEHAVIOURAL has one form"
at "$base"; entry 0.105.1 "$b" "  - summary: s" "  - reason: r"
run_case reason-on-a-breaking-bullet 1
expect reason-on-a-breaking-bullet "reason: belongs to a BEHAVIOURAL (patch-safe) bullet only"

# Not gated: an API change and a bullet about who is affected may be in a patch (docs/api-policy.md).
at "$base"; entry 0.105.1 "- **BREAKING:** an API changes." "  - summary: an API changes."; regen
run_case api-change-in-a-patch 0
at "$base"; entry 0.105.1 "- **BREAKING (only for 0.105.0):** gone." "  - summary: gone."; regen
run_case breaking-for-one-version-in-a-patch 0

# No tag at all: nothing to compare with.
at "$base"; g tag -d v0.105.0 >/dev/null; entry 0.105.1 "$b" "  - summary: a thing changes."; regen
run_case behavioural-change-with-no-tag 0
g tag v0.105.0 "$base"

# 4. A SECURITY bullet has to settle the floor, in the same change: raise min_safe to this release,
# or record that it stays (min_safe_unchanged) with the reason.
write_compat() { # <min_safe> <min_safe_date> <reasons items> <min_safe_unchanged items>
  cat >compat/compat.json <<JSON
{
  "schema": 1,
  "min_safe": "$1",
  "min_safe_date": "$2",
  "reasons": [$3],
  "min_safe_unchanged": [$4],
  "note": "selftest"
}
JSON
}
old_reason='{"version": "v0.102.2", "summary": "an old fix"}'
sec='- **SECURITY:** a header is no longer forwarded.'
secf='  - summary: a header is no longer forwarded.'

at "$base"; entry 0.105.1 "$sec" "$secf"; regen
write_compat v0.102.2 2026-09-27T15:03:06Z "$old_reason" ''   # a fixture: the floor of the real compat.json moves, this case names it
run_case security-without-a-decision 1
expect security-without-a-decision "SECURITY of v0.105.1 (CHANGELOG.md line 7): compat/compat.json has to settle what the fix does to the floor"
expect security-without-a-decision "The floor is v0.102.2"

at "$base"; entry 0.105.1 "$sec" "$secf"; regen
write_compat v0.102.2 2026-09-27T15:03:06Z "$old_reason" '{"version": "v0.105.1", "reason": "a latent hole; a typical host cannot reach it"}'
run_case security-with-an-unchanged-record 0

at "$base"; entry 0.105.1 "$sec" "$secf" "$sec" "  - summary: and another."; regen
write_compat v0.102.2 2026-09-27T15:03:06Z "$old_reason" '{"version": "v0.105.1", "reason": "both are latent"}'
run_case two-security-bullets-one-record 0

at "$base"; entry 0.105.1 "$sec" "$secf"; regen
write_compat v0.102.2 2026-09-27T15:03:06Z "$old_reason" '{"version": "v0.105.9", "reason": "decided for the number this had before it was renumbered"}'
run_case unchanged-record-for-another-release 1
expect unchanged-record-for-another-release "has to settle what the fix does to the floor"

at "$base"; entry 0.105.1 "$sec" "$secf"; regen
write_compat v0.102.2 2026-09-27T15:03:06Z "$old_reason" '{"version": "v0.105.1", "reason": "  "}'
run_case unchanged-record-without-a-reason 1
expect unchanged-record-without-a-reason "has no reason"

at "$base"; entry 0.105.1 "$sec" "$secf"; regen
write_compat v0.105.1 2026-10-20T10:00:00Z "$old_reason, {\"version\": \"v0.105.1\", \"summary\": \"a header is no longer forwarded\"}" ''
run_case floor-raised-to-this-release 0

at "$base"; entry 0.105.1 "$sec" "$secf"; regen
write_compat v0.105.1 2026-10-20T10:00:00Z "$old_reason" ''
run_case floor-raised-without-the-reason-listed 1
expect floor-raised-without-the-reason-listed '"reasons" has no entry for it'

at "$base"; entry 0.105.1 "$sec" "$secf"; regen
write_compat v0.105.1 2026-09-27T15:03:06Z "$old_reason, {\"version\": \"v0.105.1\", \"summary\": \"s\"}" ''
run_case floor-raised-with-the-old-date 1
expect floor-raised-with-the-old-date "is before the entry's date 2026-10-20"

at "$base"; entry 0.105.1 "$sec" "$secf"; regen
write_compat v0.105.1 2026-10-20T10:00:00Z "$old_reason, {\"version\": \"v0.105.1\", \"summary\": \"s\"}" '{"version": "v0.105.1", "reason": "r"}'
run_case floor-raised-and-recorded-unchanged 1
expect floor-raised-and-recorded-unchanged "it is one or the other"

at "$base"; entry 0.105.1 "$sec" "$secf"; regen; echo '{' >compat/compat.json
run_case security-with-an-unreadable-compat-json 1
expect security-with-an-unreadable-compat-json "cannot read the decision"

at "$base"; entry 0.105.1 "$sec"
run_case security-without-a-summary 1
expect security-without-a-summary "no summary: line"

at "$base"; entry 0.105.1 "- **SECURITY (high):** x" "  - summary: s"
run_case security-with-a-qualifier 1
expect security-with-a-qualifier "SECURITY takes no qualifier"

# No SECURITY bullet: compat.json is not looked at, so a change that leaves it alone passes (cases above), and so does one that breaks it.
at "$base"; entry 0.105.1 "- a plain bullet"; regen; echo '{' >compat/compat.json
run_case no-security-bullet-does-not-read-compat-json 0

# 5. No go, no check: it must not pass silently.
at "$base"; entry 0.105.1 "- a plain bullet"; regen
mkdir "$tmp/nogo"
for tool in bash git grep sed sort head tail wc tr cat dirname mktemp rm awk env; do ln -s "$(command -v "$tool")" "$tmp/nogo/$tool"; done
got=0
PATH="$tmp/nogo" "$(command -v bash)" ./scripts/release-check.sh "$base" >"$tmp/out" 2>&1 || got=$?
out="$(cat "$tmp/out")"
if [ "$got" -ne 1 ]; then complain "no-go: exit=$got, want 1"; sed 's/^/    /' "$tmp/out" >&2; else echo "ok: no-go (exit 1)"; fi
expect no-go "go is needed"

exit "$fail"
