#!/usr/bin/env bash
# Counts who imports each go-common package. Regenerates the numbers in
# docs/package-usage.md.
#
#   scripts/package-usage.sh [projects-dir]
#
# projects-dir is a directory that holds the local clones of the services (default
# $HOME/Documents/GITHUB_PROJECTS). It only reads files: no network, no Go toolchain,
# and it is cheap enough for a laptop (about a minute with ripgrep once the disk
# cache is warm; plain grep is slower). Output is tab-separated, one line per
# importable package:
#
#   package  repos  repos_test_only  in_repo  in_repo_test_only
#
#   repos             distinct repositories that import it from a non-test .go file
#   repos_test_only   distinct repositories that import it only from _test.go files
#   in_repo           go-common packages that import it from non-test code
#   in_repo_test_only go-common packages that import it only from tests
#
# A "repository" is the nearest directory with a .git. Dated copies, worktrees and
# forks of one service are collapsed into one by the repository's origin URL (the
# directory name, minus a trailing -YYYYMMDD, when there is no origin), and copies of
# go-common itself are skipped. Only real import lines count, not strings or comments;
# vendor/ is ignored. Names are never printed: the output is counts only. A one-line
# total (repositories that import go-common from non-test code) goes to stderr.
#
# The count is a LOWER BOUND: it sees only what is cloned under projects-dir.
set -euo pipefail

self="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
projects="${1:-$HOME/Documents/GITHUB_PROJECTS}"
projects="${projects%/}"
[ -d "$projects" ] || { echo "package-usage: no such directory: $projects" >&2; exit 2; }

module="$(sed -n 's/^module[[:space:]]*//p' "$self/go.mod" | head -n 1 | tr -d '"[:space:]')"
[ -n "$module" ] || { echo "package-usage: no module line in $self/go.mod" >&2; exit 2; }
mod_re="$(printf '%s' "$module" | sed 's/[.]/\\./g')"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# search <dir> prints "path:line:content" for every line of a Go file under <dir>
# that mentions the module path in quotes. ripgrep is much faster; grep gives the
# same output.
search() {
  if command -v rg >/dev/null 2>&1; then
    (cd "$1" && rg --no-heading --with-filename --line-number --no-messages --hidden \
      -g '*.go' -g '!.git' -g '!vendor' -e "\"${mod_re}[^\"]*\"" . || true)
  else
    (cd "$1" && grep -rnE --include='*.go' --exclude-dir=.git --exclude-dir=vendor \
      "\"${mod_re}[^\"]*\"" . 2>/dev/null || true)
  fi
}

# imports turns search output into "dir<TAB>N|T<TAB>package" for the real import
# lines only (an optional "import", an optional alias, the quoted path, an optional
# comment), so a string or a comment that names a package is not counted. N is a
# non-test file, T a _test.go file; package is the import path below the module.
imports() {
  awk -v module="$module" '
    BEGIN {
      m = module; gsub(/[.]/, "\\.", m)
      re = "^[ \t]*(import[ \t]+)?([A-Za-z_][A-Za-z0-9_]*[ \t]+|\\.[ \t]+)?\"" m "(/[^\"]*)?\"[ \t]*(//.*)?$"
    }
    {
      i = index($0, ":"); f = substr($0, 1, i - 1); rest = substr($0, i + 1)
      j = index(rest, ":"); c = substr(rest, j + 1)
      if (c !~ re) next
      match(c, /"[^"]*"/); p = substr(c, RSTART + 1, RLENGTH - 2)
      p = substr(p, length(module) + 1); sub(/^\//, "", p)
      sub(/^\.\//, "", f)
      d = f; if (!sub(/\/[^\/]*$/, "", d)) d = "."
      printf "%s\t%s\t%s\n", d, (f ~ /_test\.go$/ ? "T" : "N"), p
    }'
}

# ---- importable packages: directories with non-test Go files, minus the ones the
# package index also skips (internal, scripts, lint, docs, testdata, hidden).
(cd "$self" && find . -name '*.go' ! -name '*_test.go' -print |
  grep -Ev '/(internal|scripts|lint|docs|testdata|vendor)/|/[._][^/]*/' |
  sed -E 's#^\./##; s#/[^/]*$##' | grep -Fvx '.' | sort -u) >"$tmp/pkgs"

# ---- go-common's own importers, one line per (importing package, imported package).
search "$self" | imports | awk -F'\t' '
  { d = $1; p = $3
    if (p == "" || d == p) next
    k = d "|" p
    if ($2 == "T") { if (!(k in seen)) seen[k] = "T" } else seen[k] = "N" }
  END { for (k in seen) { split(k, a, "|"); print a[2] "\t" seen[k] } }' | sort >"$tmp/inrepo"

# ---- the fleet: map each importing directory to a repository identity.
search "$projects" | imports >"$tmp/fleet-files"
cut -f1 "$tmp/fleet-files" | sort -u >"$tmp/dirs"

: >"$tmp/dir-repo"
while IFS= read -r d; do
  r="$projects/$d"
  while [ "$r" != "$projects" ] && [ "$r" != "/" ] && [ ! -e "$r/.git" ]; do
    r="${r%/*}"
  done
  id=""
  if [ -e "$r/.git" ]; then
    url="$(git -C "$r" config --get remote.origin.url 2>/dev/null || true)"
    if [ -n "$url" ]; then
      # git@host:owner/name.git and https://[user@]host/owner/name.git alike.
      id="$(printf '%s' "$url" | sed -E 's#^[a-z+]+://([^@/]*@)?[^/]*/##; s#^[^@/]*@[^:]*:##; s#\.git$##' |
        tr 'A-Z_' 'a-z-')"
    else
      id="dir:$(basename "$r" | sed -E 's/-[0-9]{8}[a-z]?$//' | tr 'A-Z_' 'a-z-')"
    fi
  else
    id="dir:$(printf '%s' "${d%%/*}" | tr 'A-Z_' 'a-z-')"
  fi
  printf '%s\t%s\n' "$d" "$id" >>"$tmp/dir-repo"
done <"$tmp/dirs"

# The repository that is go-common itself (its copies and worktrees) is not a consumer.
self_id="$(printf '%s' "$module" | sed -E 's#^[^/]*/##' | tr 'A-Z_' 'a-z-')"

awk -F'\t' -v self_id="$self_id" -v summary="$tmp/summary" '
  FILENAME == ARGV[1] { repo[$1] = $2; next }
  { id = repo[$1]; p = $3
    if (id == "" || id == self_id || p == "") next
    k = id "|" p
    if ($2 == "T") { if (!(k in seen)) seen[k] = "T" } else { seen[k] = "N"; consumers[id] = 1 } }
  END { for (k in seen) { split(k, a, "|"); print a[2] "\t" seen[k] }
        n = 0; for (id in consumers) n++; print n > summary }' \
  "$tmp/dir-repo" "$tmp/fleet-files" | sort >"$tmp/fleet"
echo "package-usage: $(cat "$tmp/summary") repositories import go-common from non-test code" >&2

# ---- one line per package.
awk -F'\t' '
  FILENAME ~ /pkgs$/   { order[++n] = $0; next }
  FILENAME ~ /inrepo$/ { if ($2 == "N") ir[$1]++; else irt[$1]++; next }
  FILENAME ~ /fleet$/  { if ($2 == "N") fr[$1]++; else frt[$1]++; next }
  END { for (i = 1; i <= n; i++) { p = order[i]
          printf "%s\t%d\t%d\t%d\t%d\n", p, fr[p], frt[p], ir[p], irt[p] } }' \
  "$tmp/pkgs" "$tmp/inrepo" "$tmp/fleet"
