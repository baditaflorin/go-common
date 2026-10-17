#!/usr/bin/env bash
# Prepare a security backport onto an old release line. Pushes nothing.
#
#   scripts/backport.sh [--dry-run] <line> <commit>...
#   scripts/backport.sh 0.97 9d2dc81
#
# <line> is MAJOR.MINOR. The script finds the newest tag of that line, creates the
# branch release/<line> from it in a NEW worktree next to this checkout (so the
# working tree you are in is not touched), and cherry-picks the commits with -x. It
# then prints what is left to do by hand: the CHANGELOG.md entry, the CI run, the
# tag, and the compat.json floor for the line. See docs/releasing.md.
#
# Stops at the first conflict and leaves the worktree for you to resolve. Backports
# of a dependency bump usually conflict in go.mod/go.sum: resolve them there.
set -euo pipefail

dry_run=0
if [ "${1:-}" = "--dry-run" ]; then dry_run=1; shift; fi
if [ "$#" -lt 2 ]; then
  sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
fi
line="$1"; shift
if ! printf '%s' "$line" | grep -Eq '^[0-9]+\.[0-9]+$'; then
  echo "backport: <line> must be MAJOR.MINOR, got '$line'" >&2
  exit 2
fi

cd "$(git rev-parse --show-toplevel)"
git fetch -q --tags origin 2>/dev/null || true

tag="$(git tag --list "v${line}.*" --sort=-v:refname | head -1)"
if [ -z "$tag" ]; then
  echo "backport: no release tag found for line v${line}.x" >&2
  exit 1
fi
patch="${tag##*.}"
next="v${line}.$((patch + 1))"
branch="release/${line}"
dir="../$(basename "$PWD")-release-${line}"

for c in "$@"; do
  git rev-parse -q --verify "${c}^{commit}" >/dev/null || { echo "backport: unknown commit '$c'" >&2; exit 1; }
done

echo "backport: base ${tag}, new version ${next}, branch ${branch}, worktree ${dir}"
printf 'backport: cherry-pick:\n'
for c in "$@"; do printf '  %s\n' "$(git log -1 --format='%h %s' "$c")"; done
if [ "$dry_run" = 1 ]; then
  echo "backport: dry run, nothing created"
  exit 0
fi

if git rev-parse -q --verify "refs/heads/${branch}" >/dev/null; then
  echo "backport: branch ${branch} already exists; delete it or continue on it by hand" >&2
  exit 1
fi
git worktree add -q -b "$branch" "$dir" "$tag"
cd "$dir"
for c in "$@"; do
  if ! git cherry-pick -x "$c"; then
    cat >&2 <<MSG

backport: $c did not apply cleanly onto ${tag}.
Resolve the conflict in ${dir} (git status; edit; git add; git cherry-pick --continue),
then carry on with the steps below. To give up: git worktree remove --force ${dir} && git branch -D ${branch}
MSG
    exit 1
  fi
done

cat <<MSG

backport: ${branch} is ready in ${dir} (${next} not yet tagged). By hand, in that worktree:
  1. Add a CHANGELOG.md entry at the top:  ## [${next#v}] - $(date -u +%F)  with a "### Fixed" bullet naming what was backported.
  2. Build and test it the way CI does (on a build host, not a laptop).
  3. Commit, push the branch, let CI pass, then tag ${next} and push the tag.
  4. Add  "v${line}": "${next}"  to "lines" in compat/compat.json on main, so services on
     this line stop being reported below the floor once they take ${next}.
MSG
