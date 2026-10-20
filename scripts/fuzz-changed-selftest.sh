#!/usr/bin/env bash
# Tests scripts/fuzz-changed.sh in a throwaway git repository, with a fake `go` on PATH, so
# it needs no toolchain and takes a few seconds. For each kind of change it checks which Fuzz
# targets scripts/fuzz.sh is asked to run:
#   - a change in a package with targets runs those, and no other package's;
#   - a docs change, a package without targets, a nested module and a function that only
#     looks like a target (`Fuzzy`) run nothing, and still pass;
#   - go.mod, go.sum, scripts/fuzz.sh, FUZZ_ALL=1 and a base that cannot be found run every
#     package;
#   - a pull request is compared with the MERGE BASE, not the moved-on tip of its target;
#   - untracked files, renames and deletions count.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"

# The fake go. FAKE_PKGS lists the package directories ./... covers; FAKE_TARGETS, as
# "dir:Target,Target dir:Target", the Fuzz targets each has. `test -list` answers the way
# Go does (names, then an ok line per package); a fuzz run appends "dir:Target" to FAKE_LOG.
cat >"$tmp/bin/go" <<'FAKE'
#!/usr/bin/env bash
mod=example.com/m
case " $* " in
  *" -list "*)
    wanted=()
    for arg in "$@"; do
      case "$arg" in
        ./...) for d in $FAKE_PKGS; do wanted+=("$d"); done ;;
        ./*) wanted+=("${arg#./}") ;;
      esac
    done
    for dir in "${wanted[@]}"; do
      for entry in $FAKE_TARGETS; do
        if [ "${entry%%:*}" = "$dir" ]; then
          names="${entry#*:}"
          printf '%s\n' ${names//,/ }
        fi
      done
      printf 'ok  \t%s/%s\t0.001s\n' "$mod" "$dir"
    done
    exit 0 ;;
esac
pattern=""
for arg in "$@"; do
  case "$arg" in -fuzz=*) pattern="${arg#-fuzz=^}"; pattern="${pattern%\$}" ;; esac
done
eval "last=\${$#}"
printf '%s:%s\n' "${last#"$mod"/}" "$pattern" >>"$FAKE_LOG"
printf 'PASS\n'
FAKE
chmod +x "$tmp/bin/go"

# A repository shaped like go-common: packages with and without Fuzz targets, a package
# nested in another, a nested module, a fixture under testdata/.
repo="$tmp/repo"
mkdir "$repo"
cd "$repo"
git init -q -b main
git config user.name selftest
git config user.email selftest@example.com
mkdir -p scripts/lib a/testdata/fuzz/FuzzA1 b c d/e g nested docs
cp "$root/scripts/fuzz.sh" "$root/scripts/fuzz-changed.sh" scripts/
cp "$root/scripts/lib/base-ref.sh" scripts/lib/
printf 'module example.com/m\n\ngo 1.26.0\n' >go.mod
: >go.sum
printf '# Changelog\n' >CHANGELOG.md
printf 'docs\n' >docs/readme.md
printf 'package a\n' >a/a.go
printf 'package a\n\nfunc FuzzA1(f *testing.F) {}\nfunc FuzzA2(f *testing.F) {}\n' >a/a_fuzz_test.go
printf 'seed\n' >a/testdata/fuzz/FuzzA1/seed1
printf 'package a\n\nfunc FuzzIgnored(f *testing.F) {}\n' >a/testdata/ignored.go
printf 'package b\n' >b/b.go
printf 'package b\n\nfunc FuzzB(f *testing.F) {}\n' >b/b_test.go
printf 'package c\n' >c/c.go
printf 'package c\n\nfunc TestC(t *testing.T) {}\n' >c/c_test.go
printf 'package d\n' >d/d.go
printf 'package e\n' >d/e/e.go
printf 'package e\n\nfunc FuzzE(f *testing.F) {}\n' >d/e/e_test.go
printf 'package g\n' >g/g.go
printf 'package g\n\nfunc Fuzzy(x int) int { return x }\n' >g/g_test.go
printf 'module example.com/nested\n\ngo 1.26.0\n' >nested/go.mod
printf 'package nested\n' >nested/x.go
printf 'package nested\n\nfunc FuzzN(f *testing.F) {}\n' >nested/x_test.go
git add -A
git commit -q -m base
base="$(git rev-parse HEAD)"

export FAKE_PKGS="a b c d d/e g"
export FAKE_TARGETS="a:FuzzA1,FuzzA2 b:FuzzB d/e:FuzzE"
export FAKE_LOG="$tmp/fuzz.log"
export PATH="$tmp/bin:$PATH"
every="a:FuzzA1 a:FuzzA2 b:FuzzB d/e:FuzzE"

fail=0
# check <name> <expected targets, or "none"> <exit code wanted> <command...>: runs the command
# (scripts/fuzz-changed.sh with some arguments) from the repository and compares the targets
# the fake go was asked to fuzz. The tree is put back afterwards.
check() {
  local name="$1" want="$2" want_exit="$3" got exit_code=0
  shift 3
  : >"$FAKE_LOG"
  "$@" >"$tmp/out.log" 2>&1 || exit_code=$?
  got="$(sort "$FAKE_LOG" | paste -sd' ' -)"
  : "${got:=none}"
  if [ "$got" != "$want" ] || [ "$exit_code" -ne "$want_exit" ]; then
    echo "FAIL: $name: fuzzed [$got] with exit $exit_code, want [$want] with exit $want_exit" >&2
    sed 's/^/    /' "$tmp/out.log" >&2
    fail=1
  else
    echo "ok: $name -> $got"
  fi
  git reset -q --hard "$base"
  git clean -qfdx
}
fuzz() { scripts/fuzz-changed.sh 5s "$base"; }
# Edits are made with plain shell and committed or not as the case needs: the script reads
# the working tree, which is what a CI checkout is. A shell script gets a shell comment.
edit() {
  case "$1" in
    *.sh) printf '# changed\n' >>"$1" ;;
    *) printf '// changed\n' >>"$1" ;;
  esac
}

edit docs/readme.md; edit CHANGELOG.md
check "docs and changelog only" none 0 fuzz

edit c/c.go
check "a package without targets" none 0 fuzz

edit a/a.go
check "a package with targets runs all of them, and no other" "a:FuzzA1 a:FuzzA2" 0 fuzz

printf 'crash\n' >a/testdata/fuzz/FuzzA1/new-seed
git add -A
check "a committed regression seed re-runs its package" "a:FuzzA1 a:FuzzA2" 0 fuzz

edit d/e/e.go
check "a package nested in another package runs on its own" "d/e:FuzzE" 0 fuzz

edit d/d.go
check "the outer package is touched, the inner one is not" none 0 fuzz

edit a/a.go; edit b/b.go
check "two packages" "a:FuzzA1 a:FuzzA2 b:FuzzB" 0 fuzz

edit nested/x.go
check "a nested module is not part of ./..." none 0 fuzz

edit g/g_test.go
check "a function named Fuzzy is not a target" none 0 fuzz

git mv b/b_test.go b/b2_test.go
check "a renamed file counts" "b:FuzzB" 0 fuzz

git rm -q a/a.go
check "a deleted file counts" "a:FuzzA1 a:FuzzA2" 0 fuzz

git rm -q -r c
check "a deleted package is not an error" none 0 fuzz

mkdir f
printf 'package f\n' >f/f.go
printf 'package f\n\nfunc FuzzF(f *testing.F) {}\n' >f/f_test.go
FAKE_PKGS="a b c d d/e g f" FAKE_TARGETS="$FAKE_TARGETS f:FuzzF" check "untracked files count" "f:FuzzF" 0 fuzz

edit go.mod
check "go.mod runs everything" "$every" 0 fuzz
edit go.sum
check "go.sum runs everything" "$every" 0 fuzz
edit scripts/fuzz.sh
check "scripts/fuzz.sh runs everything" "$every" 0 fuzz
edit scripts/fuzz-changed.sh
check "a change to this script's own file maps to no package" none 0 fuzz

edit docs/readme.md
check "FUZZ_ALL=1 runs everything" "$every" 0 env FUZZ_ALL=1 scripts/fuzz-changed.sh 5s "$base"
check "a base that cannot be found runs everything" "$every" 0 scripts/fuzz-changed.sh 5s no-such-ref

edit a/a.go
check "a dry run runs nothing" none 0 env FUZZ_DRY_RUN=1 scripts/fuzz-changed.sh 5s "$base"
grep -q 'dry run, would fuzz ./a for 5s each' "$tmp/out.log" || { echo "FAIL: the dry run does not say what it would fuzz" >&2; sed 's/^/    /' "$tmp/out.log" >&2; fail=1; }

# A pull request is compared with the merge base. The target branch moves on (b changes
# on main) after the pull request branched and changed a: only a may be fuzzed.
git clone -q --bare "$repo" "$tmp/origin.git"
git remote add origin "$tmp/origin.git"
git checkout -q -b main-moves "$base"
edit b/b.go
git commit -q -am "main moves on"
git push -q origin main-moves:main
git checkout -q --detach "$base"
edit a/a.go
git commit -q -am "the pull request"
git update-ref -d refs/remotes/origin/main 2>/dev/null || true
check "a pull request is compared with its merge base" "a:FuzzA1 a:FuzzA2" 0 \
  env CI=woodpecker CI_PIPELINE_EVENT=pull_request CI_COMMIT_TARGET_BRANCH=main scripts/fuzz-changed.sh 5s

exit "$fail"
