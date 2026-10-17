#!/usr/bin/env bash
# API compatibility gate for go-common.
#
# Compares the exported API of HEAD with a base ref using golang.org/x/exp's
# apidiff, and fails when it finds an incompatible change that CHANGELOG.md does
# not call out as BREAKING in the same change. See docs/api-policy.md.
#
#   scripts/apidiff-check.sh [base-ref]
#
# The base is chosen by scripts/lib/base-ref.sh: the argument or $BASE_REF, else
# in a pull request the merge base with the target branch, else HEAD^ in CI, else
# the newest v* tag.
#
# apidiff sees API only. It cannot see a behaviour change (a changed default, a
# removed env var, a different error), so a green run is not a promise of
# compatibility. Declare those in CHANGELOG.md with **BREAKING:** too.
#
# The base tree is read with `git archive`, so no worktree is created and the
# repository is left untouched. Needs git, go and (installed on demand) apidiff.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=scripts/tool-versions.sh
source scripts/tool-versions.sh

module="$(go list -m)"

# shellcheck source=scripts/lib/base-ref.sh
source scripts/lib/base-ref.sh
if ! base="$(resolve_base_commit "${1:-}")"; then
  echo "apidiff-check: could not determine the base to compare with" >&2
  exit 1
fi

if ! command -v apidiff >/dev/null 2>&1; then
  echo "apidiff-check: installing apidiff@${APIDIFF_VERSION}"
  go install "golang.org/x/exp/cmd/apidiff@${APIDIFF_VERSION}"
  PATH="$(go env GOPATH)/bin:$PATH"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/base"
git archive "$base" | tar -x -C "$tmp/base"

if [ ! -f "$tmp/base/go.mod" ]; then
  echo "apidiff-check: base ${base} has no go.mod, nothing to compare"
  exit 0
fi

echo "apidiff-check: ${module} HEAD vs $(git rev-parse --short "$base")"
# apidiff chatters on stderr ("Ignoring internal package ..."); keep stderr out of
# the report, and show it only if apidiff really failed.
if ! (cd "$tmp/base" && apidiff -m -w "$tmp/base.export" "$module") 2>"$tmp/apidiff.err"; then
  cat "$tmp/apidiff.err" >&2
  echo "apidiff-check: apidiff failed to read the base API" >&2
  exit 1
fi

# -incompatible keeps only the changes that can break a caller; additions are
# compatible and need no ceremony.
if ! report="$(apidiff -m -incompatible "$tmp/base.export" "$module" 2>"$tmp/apidiff.err")"; then
  cat "$tmp/apidiff.err" >&2
  echo "apidiff-check: apidiff failed to compare the APIs" >&2
  exit 1
fi

if [ -z "$(printf '%s' "$report" | tr -d '[:space:]')" ]; then
  echo "apidiff-check: no incompatible API changes"
  exit 0
fi

echo "$report"
echo

# The change must say so: an added CHANGELOG bullet that starts with **BREAKING
# (as in "- **BREAKING:** ..." or "- **BREAKING (behavioural):** ..."). A looser
# match would be satisfied by any entry that merely mentions the word.
if git diff "${base}" -- CHANGELOG.md | grep -Eq '^\+[[:space:]]*(-[[:space:]]+)?\*\*BREAKING'; then
  echo "apidiff-check: incompatible API changes are declared BREAKING in CHANGELOG.md, allowed"
  exit 0
fi

cat >&2 <<'MSG'
apidiff-check: FAIL. This change removes or alters exported API that callers may
depend on, and CHANGELOG.md does not say so.

Either keep the old API (add the new one next to it and mark the old one with a
`// Deprecated:` comment, see docs/api-policy.md), or add a bullet starting with
**BREAKING:** to this change's CHANGELOG.md entry that names what callers must
change.
MSG
exit 1
