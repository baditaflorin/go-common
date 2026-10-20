#!/usr/bin/env bash
# Short fuzz smoke of only the Fuzz targets in the packages a change touches.
#
#   scripts/fuzz-changed.sh [fuzztime] [base-ref]   # default 5s per target
#   FUZZ_ALL=1 scripts/fuzz-changed.sh 5s           # every package, as scripts/fuzz.sh does
#   FUZZ_DRY_RUN=1 scripts/fuzz-changed.sh          # say what it would fuzz, run nothing
#
# A pull request runs this instead of scripts/fuzz.sh: fuzzing every target took about
# half of a pipeline, and most changes touch no package that has one. A push to main
# still runs scripts/fuzz.sh, so every target is mutated at least once per merge.
#
# Which packages: those with a changed file under their directory, counting the old path
# of a renamed file. The change is the diff from the base (scripts/lib/base-ref.sh picks
# it, as for the other gate scripts) to the working tree. A file under testdata/ belongs
# to the package above it, so a committed fuzz regression seed re-runs its own target.
# A package that only IMPORTS a changed package is not fuzzed: most imports are
# incidental (response, telemetry and secrets are imported by four fuzzed packages and
# no target exercises them), and `go test` replays every seed corpus on every pull
# request in the validate step.
#
# Everything is fuzzed when go.mod, go.sum or scripts/fuzz.sh changed, or
# when there is no base to compare with: a gate that silently narrows would be worse than
# one that runs long. A change that touches no package with a Fuzz target passes at once.
set -euo pipefail

fuzztime="${1:-5s}"
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Files whose change can alter what every target fuzzes.
full_triggers=(go.mod go.sum scripts/fuzz.sh)

# run_fuzz [dir...] fuzzes those package directories, or every package when given none.
run_fuzz() {
  if [ "${FUZZ_DRY_RUN:-}" = "1" ]; then
    echo "fuzz-changed: dry run, would fuzz ${*:-every package} for ${fuzztime} each"
    exit 0
  fi
  if [ "$#" -eq 0 ]; then
    exec scripts/fuzz.sh "$fuzztime"
  fi
  PKGS="$*" exec scripts/fuzz.sh "$fuzztime"
}

everything() {
  echo "fuzz-changed: $1; fuzzing every package"
  run_fuzz
}

if [ "${FUZZ_ALL:-}" = "1" ]; then
  everything "FUZZ_ALL=1"
fi

# shellcheck source=scripts/lib/base-ref.sh
source scripts/lib/base-ref.sh
base="$(resolve_base_commit "${2:-}")" || everything "no base to compare with"

# --no-renames names both sides of a rename. Untracked files count too, for a local run
# before `git add` (a CI checkout has none).
tracked="$(git diff --name-only --no-renames "$base")" || everything "git diff against ${base} failed"
changed="${tracked}"$'\n'"$(git ls-files --others --exclude-standard)"

for trigger in "${full_triggers[@]}"; do
  if grep -qxF "$trigger" <<<"$changed"; then
    everything "${trigger} changed"
  fi
done

# dirs_of turns a list of file paths into the sorted, unique directories above them.
dirs_of() { awk '{ i = match($0, /\/[^\/]*$/); print (i ? substr($0, 1, i - 1) : ".") }' | sort -u; }

# Package directories: where Go files live, except below a nested module (lint/fixture
# has its own go.mod and is not part of ./...) and inside testdata, _ and . directories,
# which the go tool skips.
nested=()
while IFS= read -r dir; do
  nested+=("$dir")
done < <(git ls-files --cached --others --exclude-standard -- '*/go.mod' | dirs_of)

declare -A is_package=()
while IFS= read -r dir; do
  case "/${dir}/" in */testdata/* | */_* | */.*) continue ;; esac
  inside_nested=0
  if [ "${#nested[@]}" -gt 0 ]; then
    for n in "${nested[@]}"; do
      case "${dir}/" in "${n}/"*) inside_nested=1 ;; esac
    done
  fi
  if [ "$inside_nested" -eq 0 ]; then
    is_package["$dir"]=1
  fi
done < <(git ls-files --cached --others --exclude-standard -- '*.go' | dirs_of)

# package_of prints the package a changed path belongs to: the nearest directory above
# it that is one. A path at the top level, or under no package, belongs to none.
package_of() {
  local dir="$1"
  while :; do
    case "$dir" in
      */*) dir="${dir%/*}" ;;
      *) return 1 ;;
    esac
    if [ -n "${is_package[$dir]:-}" ]; then
      printf '%s\n' "$dir"
      return 0
    fi
  done
}

declare -A touched=()
while IFS= read -r path; do
  [ -n "$path" ] || continue
  if pkg="$(package_of "$path")"; then
    touched["$pkg"]=1
  fi
done <<<"$changed"

# A Fuzz target is `func FuzzXxx` with Xxx not starting in lower case: what
# `go test -list '^Fuzz'` finds in scripts/fuzz.sh.
declare -A has_target=()
while IFS= read -r dir; do
  has_target["$dir"]=1
done < <(git grep --untracked -lE '^func Fuzz($|[^a-z])' -- '*_test.go' | dirs_of)

selected=()
if [ "${#touched[@]}" -gt 0 ]; then
  while IFS= read -r pkg; do
    if [ -n "${has_target[$pkg]:-}" ]; then
      selected+=("./${pkg}")
    fi
  done < <(printf '%s\n' "${!touched[@]}" | sort)
fi

echo "fuzz-changed: $(git rev-parse --short "$base") to the working tree: $(grep -c . <<<"$changed" || true) changed files in ${#touched[@]} Go packages"
if [ "${#selected[@]}" -eq 0 ]; then
  echo "fuzz-changed: none of them has a Fuzz target, so there is nothing to fuzz (scripts/fuzz.sh fuzzes every target)"
  exit 0
fi
echo "fuzz-changed: fuzzing ${selected[*]}"
run_fuzz "${selected[@]}"
