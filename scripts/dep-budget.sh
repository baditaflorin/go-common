#!/usr/bin/env bash
# Keeps go-common's small packages small: a dependency budget for the third-party
# modules each package pulls in.
#
#   scripts/dep-budget.sh              check the budget (CI runs this)
#   scripts/dep-budget.sh --print      print every importable package's third-party
#                                      modules in the budget file's syntax
#   scripts/dep-budget.sh --write-doc  regenerate docs/dependencies.md
#   scripts/dep-budget.sh --check-doc  fail if docs/dependencies.md is stale (CI runs this)
#
# go-common is one Go module, but a consumer's go.mod and go.sum only list the
# third-party modules that the packages it imports reach. So a package such as `env`
# stays cheap to import as long as it, and the go-common packages it imports, use
# nothing outside the standard library. Nothing stopped that from changing silently,
# so this script measures each package's third-party module set and fails when a
# budgeted package's set is not a subset of the modules scripts/dep-budget.txt allows
# it.
#
# The measure: `go list -deps` over the whole module, one call (about a second, no
# network once the module cache is warm). A package's set is every module other than
# this one that its non-test imports reach, directly or through other go-common
# packages; the standard library is not counted and versions are not part of it, so a
# dependency bump changes nothing here. It is taken for linux/amd64 without cgo, the
# fleet's build target, so the answer is the same on a laptop and in CI.
#
# Needs bash, awk and go (only for `go list`). scripts/dep-budget-selftest.sh tests
# this script with a stub `go`.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

budget="${DEP_BUDGET_FILE:-scripts/dep-budget.txt}"
doc="${DEP_BUDGET_DOC:-docs/dependencies.md}"

usage() {
  sed -n '5,9p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
}

mode=check
case "${1:-}" in
  "") ;;
  --print) mode=print ;;
  --write-doc) mode=doc ;;
  --check-doc) mode=doc ;;
  -h | --help) usage; exit 0 ;;
  *) usage; exit 2 ;;
esac
if [ "$#" -gt 1 ]; then
  usage
  exit 2
fi
action="${1:-}"

if [ ! -f "$budget" ]; then
  echo "dep-budget: $budget not found" >&2
  exit 1
fi
self="$(awk '$1 == "module" { gsub(/"/, "", $2); print $2; exit }' go.mod)"
if [ -z "$self" ]; then
  echo "dep-budget: no module line in go.mod" >&2
  exit 1
fi

# Fixed so that the result does not depend on the machine it runs on.
export LC_ALL=C CGO_ENABLED=0 GOOS=linux GOARCH=amd64

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# One line per package, dependencies before dependents:
#   ImportPath|Name|Module|Imports      (Module is empty for the standard library)
if ! go list -deps -f '{{.ImportPath}}|{{.Name}}|{{with .Module}}{{.Path}}{{end}}|{{join .Imports " "}}' ./... >"$tmp/graph"; then
  echo "dep-budget: 'go list -deps ./...' failed (is the module cache populated?)" >&2
  exit 1
fi

# analyze runs the awk program below over the budget file and the package graph.
# Output goes to stdout; problems go to stderr with a non-zero exit.
analyze() {
  awk -v self="$self" -v mode="$1" -v bfile="$budget" "$prog" "$budget" "$tmp/graph"
}

IFS= read -r -d '' prog <<'AWK' || true
# Inputs (see analyze() in dep-budget.sh): the budget file, then `go list` lines
#   ImportPath|Name|Module|Imports
# and the variables self (this module's path), mode (check|print|doc) and bfile.

BEGIN { FS = "|" }

function fail(msg) {
    print "dep-budget: " msg > "/dev/stderr"
    bad = 1
}

# addset adds module m to the third-party set of package p.
function addset(p, m) {
    if (!((p SUBSEP m) in S)) {
        S[p, m] = 1
        L[p] = L[p] " " m
    }
}

# sorted returns the words of a space-separated string in byte order.
function sorted(str,    a, n, i, j, t, out) {
    n = split(str, a, " ")
    for (i = 2; i <= n; i++) {
        t = a[i]
        for (j = i - 1; j >= 1 && a[j] > t; j--) a[j + 1] = a[j]
        a[j + 1] = t
    }
    out = ""
    for (i = 1; i <= n; i++) out = out (i > 1 ? " " : "") a[i]
    return out
}

# rel is a first-party import path relative to the module root.
function rel(p) {
    return (p == self) ? "." : substr(p, length(self) + 2)
}

# importable reports whether a package, given by its path relative to the module
# root, is one consumers import: not internal, a test helper, a script, lint
# fixture, doc or testdata, and not hidden.
function importable(r,    n, el, i) {
    n = split(r, el, "/")
    for (i = 1; i <= n; i++) {
        if (el[i] ~ /^(internal|testhelpers|scripts|lint|docs|testdata|vendor)$/ || el[i] ~ /^[._]/) return 0
    }
    return 1
}

# bfs fills Q[1..QN] with the go-common packages root reaches (root first, nearest
# first) and PAR with the way back, so chain() can say how root gets to one of them.
function bfs(root,    head, p, k, a, i, q) {
    for (q in SEEN) delete SEEN[q]
    QN = 1; Q[1] = root; SEEN[root] = 1; PAR[root] = ""
    for (head = 1; head <= QN; head++) {
        p = Q[head]
        k = split(IMPS[p], a, " ")
        for (i = 1; i <= k; i++) {
            q = a[i]
            if ((q in MOD) && MOD[q] == self && !(q in SEEN)) {
                SEEN[q] = 1; PAR[q] = p; Q[++QN] = q
            }
        }
    }
}

function chain(f,    s) {
    s = rel(f)
    while (PAR[f] != "") { f = PAR[f]; s = rel(f) " -> " s }
    return s
}

# allowed reports whether the budget of pkg lets it reach module m, either on its
# own lines or through an @package it includes.
function allowed(pkg, m, depth,    a, k, i) {
    if ((pkg SUBSEP m) in allow) return 1
    if (depth > 8) return 0
    k = split(INC[pkg], a, " ")
    for (i = 1; i <= k; i++) if (allowed(a[i], m, depth + 1)) return 1
    return 0
}

function plural(n, one, many) {
    return n " " (n == 1 ? one : many)
}

# Budget file: "package [module | @package ...]", any number of lines per package,
# # comments. `@other` allows whatever the budget of package `other` allows.
FILENAME == bfile {
    line = $0
    sub(/#.*/, "", line)
    k = split(line, f, " ")
    if (k == 0) next
    if (!(f[1] in budgeted)) { budgeted[f[1]] = FNR; border[++bn] = f[1] }
    for (i = 2; i <= k; i++) {
        if (substr(f[i], 1, 1) == "@") {
            INC[f[1]] = INC[f[1]] " " substr(f[i], 2)
            INCS[++ni] = f[1] " " substr(f[i], 2)
        } else if (!((f[1] SUBSEP f[i]) in allow)) {
            allow[f[1], f[i]] = 1; AL[f[1]] = AL[f[1]] " " f[i]
        }
    }
    next
}

{
    if (NF != 4) { fail("unexpected line from go list: " $0); next }
    n++
    ORDER[n] = $1; NAME[$1] = $2; MOD[$1] = $3; IMPS[$1] = $4
}

END {
    if (bad) exit 1
    if (bn == 0) { fail(bfile " lists no packages"); exit 1 }
    for (i = 1; i <= ni; i++) {
        split(INCS[i], f, " ")
        if (!(f[2] in budgeted)) fail("scripts/dep-budget.txt: " f[1] " includes @" f[2] ", which has no line of its own")
    }
    if (bad) exit 1

    # Package sets. `go list -deps` prints dependencies first, so every import's set is
    # complete by the time its importer is reached. DL[p] is the modules p imports
    # itself, as opposed to the ones its imports bring in.
    for (i = 1; i <= n; i++) {
        p = ORDER[i]
        if (MOD[p] == "") continue
        k = split(IMPS[p], a, " ")
        for (j = 1; j <= k; j++) {
            q = a[j]
            if (!(q in MOD) || MOD[q] == "") continue
            if (MOD[q] != self) {
                addset(p, MOD[q])
                if (MOD[p] == self && !((p SUBSEP MOD[q]) in DS)) { DS[p, MOD[q]] = 1; DL[p] = DL[p] " " MOD[q] }
            }
            u = split(L[q], b, " ")
            for (x = 1; x <= u; x++) addset(p, b[x])
        }
    }

    # The importable packages, in byte order.
    for (i = 1; i <= n; i++) {
        p = ORDER[i]
        if (MOD[p] != self || NAME[p] == "main") continue
        r = rel(p)
        if (r != "." && !importable(r)) continue
        PK[++np] = r; PATH[r] = p
    }
    for (i = 2; i <= np; i++) {
        t = PK[i]
        for (j = i - 1; j >= 1 && PK[j] > t; j--) PK[j + 1] = PK[j]
        PK[j + 1] = t
    }

    if (mode == "print") {
        for (i = 1; i <= np; i++) {
            s = sorted(L[PATH[PK[i]]])
            k = split(s, a, " ")
            if (k == 0) print PK[i]
            for (j = 1; j <= k; j++) print PK[i], a[j]
        }
        exit 0
    }
    if (mode == "doc") { render(); exit 0 }

    # mode == check
    for (i = 1; i <= bn; i++) {
        pkg = border[i]
        if (!(pkg in PATH)) {
            fail("scripts/dep-budget.txt names " pkg ", which is not an importable package of this module; remove or rename the line")
            continue
        }
        P = PATH[pkg]
        bfs(P)
        k = split(sorted(L[P]), mods, " ")
        direct = ""; indirect = ""; nnew = 0
        for (j = 1; j <= k; j++) {
            m = mods[j]
            if (allowed(pkg, m, 0)) continue
            nnew++
            # Who brought it in: the go-common packages that import it, nearest first.
            who = ""; more = 0
            for (h = 1; h <= QN; h++) {
                if ((Q[h] SUBSEP m) in DS) { if (who == "") who = Q[h]; else more++ }
            }
            if (who == "") { indirect = indirect (indirect == "" ? "" : ", ") m; continue }
            if (who == P) why = "imported by " pkg " itself"
            else why = "imported by " rel(who) " (" chain(who) ")"
            if (more > 0) why = why ", and by " plural(more, "other go-common package", "other go-common packages")
            direct = direct "        " m "\n            " why "\n"
        }
        if (nnew > 0) {
            viol = viol "    " pkg ": " plural(nnew, "new third-party module", "new third-party modules") "\n" direct
            if (indirect != "") viol = viol "        reached only through other third-party modules (the ones above, or a dependency upgrade): " indirect "\n"
            nviol += nnew; VP[pkg] = 1
        }
        # Slack: modules the budget allows that the package no longer reaches.
        k = split(sorted(AL[pkg]), al, " ")
        for (j = 1; j <= k; j++) {
            if (!((P SUBSEP al[j]) in S)) slack = slack "    " pkg ": no longer reaches " al[j] "\n"
        }
    }
    if (nviol > 0) {
        np2 = 0; for (q in VP) np2++
        printf "dep-budget: FAIL: %s over budget (%s)\n\n", plural(np2, "package is", "packages are"), plural(nviol, "new module", "new modules") > "/dev/stderr"
        printf "%s\n", viol > "/dev/stderr"
        printf "What to do:\n" > "/dev/stderr"
        printf "  1. Usually, keep the package small: move the code that needs the new module into a\n" > "/dev/stderr"
        printf "     package that already depends on it (or a new one), or drop the import. Consumers\n" > "/dev/stderr"
        printf "     of a small package should not inherit another module, and that module's own\n" > "/dev/stderr"
        printf "     dependencies, in their go.mod and in every scanner alert.\n" > "/dev/stderr"
        printf "  2. If the dependency is intended: add the module to the package's line in\n" > "/dev/stderr"
        printf "     scripts/dep-budget.txt, with the reason in the pull request description, then run\n" > "/dev/stderr"
        printf "     scripts/dep-budget.sh --write-doc and commit docs/dependencies.md.\n" > "/dev/stderr"
        printf "  `scripts/dep-budget.sh --print` shows every package's current set in the budget's syntax.\n" > "/dev/stderr"
    }
    if (bad || nviol > 0) exit 1
    printf "dep-budget: ok: %d budgeted packages are within budget (%d importable packages measured)\n", bn, np
    if (slack != "") {
        printf "dep-budget: note: some budget lines allow more than the package reaches today; tighten them in\n"
        printf "scripts/dep-budget.txt (a ratchet only holds if it follows the set down):\n%s", slack
    }
}

# render writes docs/dependencies.md to stdout.
function render(    i, j, k, a, P, s, via, h, m, kn, nbud, nzero, pk, p, r, mods) {
    for (i = 1; i <= np; i++) {
        P = PATH[PK[i]]
        CNT[PK[i]] = split(L[P], a, " ")
        if (PK[i] in budgeted) nbud++
        if (CNT[PK[i]] == 0) nzero++
    }
    print "<!-- Code generated by `scripts/dep-budget.sh --write-doc`; DO NOT EDIT. -->"
    print ""
    print "# Third-party dependencies per package"
    print ""
    printf "%d importable packages, %d of them with no third-party module at all, and %d under a budget. ", np, nzero, nbud
    print "Generated from `go list -deps`; CI fails if this file is stale."
    print ""
    print "go-common is one Go module, but a service only builds, and only lists in its own `go.mod` and `go.sum`, the"
    print "third-party modules that the packages it imports reach. This page measures that for every package, so that"
    print "the small packages many services import can be kept small. `scripts/dep-budget.sh` enforces it for the"
    print "budgeted ones; see [How to change a budget](#how-to-change-a-budget). The measurements behind the budget, and why"
    print "go-common is not split into nested modules, are in [ADR-0002](adr/0002-dependency-budget.md)."
    print ""
    print "## How to read it"
    print ""
    print "- **Modules** is the number of distinct third-party modules (any module other than go-common itself; the"
    print "  standard library is not counted) in the package's transitive, non-test imports on linux/amd64 without cgo. It"
    print "  includes modules reached through other go-common packages, which is the point: `env` stays small only while"
    print "  everything `env` imports does. A service's `go mod tidy` lists these modules as indirect requirements (plus any"
    print "  that only another platform needs). Versions are not part of the measure, so a dependency bump leaves this page unchanged."
    print "- **0** means the package imports only the standard library and go-common packages that do the same."
    print "- **Via** names the go-common packages, within the package's own imports, that import third-party modules"
    print "  themselves: those imports are what to move or drop to shrink the number. `(itself)` is the package itself."
    print "- **Budget** is `yes` for a package under `scripts/dep-budget.txt`."
    print "- A module reached only through another third-party module (gRPC through the OpenTelemetry exporter, for"
    print "  example) has no importer of its own: the section [Direct imports](#direct-imports) shows who brings in what."
    print ""
    print "## Packages"
    print ""
    print "Heaviest first, then by name."
    print ""
    print "| Package | Modules | Budget | Via | Third-party modules reached |"
    print "| --- | --- | --- | --- | --- |"
    for (i = 1; i <= np; i++) KEYS[i] = sprintf("%05d|%s", 99999 - CNT[PK[i]], PK[i])
    sortkeys(KEYS, np)
    for (i = 1; i <= np; i++) {
        pk = substr(KEYS[i], 7)
        P = PATH[pk]
        bfs(P)
        via = ""
        for (h = 1; h <= QN; h++) {
            if (DL[Q[h]] != "") via = via (via == "" ? "" : ", ") "`" ((Q[h] == P) ? "(itself)" : rel(Q[h])) "`"
        }
        s = ""
        k = split(sorted(L[P]), a, " ")
        for (j = 1; j <= k; j++) s = s (j > 1 ? ", " : "") "`" a[j] "`"
        printf "| [`%s`](../%s/) | %d | %s | %s | %s |\n", pk, pk, CNT[pk], (pk in budgeted) ? "yes" : "-", (via == "") ? "-" : via, (s == "") ? "-" : s
    }

    # Direct imports: which go-common package imports which third-party module itself.
    print ""
    print "## Direct imports"
    print ""
    print "The go-common packages that import third-party modules themselves, and which. Every other package gets its"
    print "modules from these, so this is where a dependency enters the module."
    print ""
    print "| Package | Third-party modules it imports directly |"
    print "| --- | --- |"
    kn = 0
    for (i = 1; i <= n; i++) {
        p = ORDER[i]
        if (MOD[p] != self || DL[p] == "") continue
        r = rel(p)
        if (r ~ /(^|\/)(testhelpers|scripts|lint|docs|testdata|vendor)(\/|$)/ || NAME[p] == "main") continue
        KEYS[++kn] = r
    }
    sortkeys(KEYS, kn)
    for (i = 1; i <= kn; i++) {
        s = ""
        k = split(sorted(DL[self "/" KEYS[i]]), a, " ")
        for (j = 1; j <= k; j++) s = s (j > 1 ? ", " : "") "`" a[j] "`"
        printf "| `%s` | %s |\n", KEYS[i], s
    }

    # Reach: which importable packages reach each module. Modules reached by exactly
    # the same packages travel together, so they share a row.
    print ""
    print "## Modules by reach"
    print ""
    print "Which packages reach which modules, widest reach first. Modules that exactly the same packages reach share a row:"
    print "they enter together, so they leave together. A module reached by many packages is one advisory away from a"
    print "release that touches all of them."
    print ""
    print "| Packages | Modules | Reached by |"
    print "| --- | --- | --- |"
    for (i = 1; i <= np; i++) {
        P = PATH[PK[i]]
        k = split(sorted(L[P]), a, " ")
        for (j = 1; j <= k; j++) {
            m = a[j]
            REACH[m] = REACH[m] (REACH[m] == "" ? "" : ", ") "`" PK[i] "`"
            REACHN[m]++
        }
    }
    for (m in REACH) {
        if (!(REACH[m] in GROUP)) { GROUP[REACH[m]] = ""; GN[REACH[m]] = REACHN[m] }
        GROUP[REACH[m]] = GROUP[REACH[m]] " " m
    }
    kn = 0
    for (r in GROUP) KEYS[++kn] = sprintf("%05d|%s", 99999 - GN[r], sorted(GROUP[r]) "|" r)
    sortkeys(KEYS, kn)
    for (i = 1; i <= kn; i++) {
        split(substr(KEYS[i], 7), a, "|")
        k = split(a[1], mods, " ")
        s = ""
        for (j = 1; j <= k; j++) s = s (j > 1 ? ", " : "") "`" mods[j] "`"
        printf "| %d | %s | %s |\n", 99999 - substr(KEYS[i], 1, 5), s, a[2]
    }

    print ""
    print "## How to change a budget"
    print ""
    print "`scripts/dep-budget.txt` lists, for each budgeted package, the third-party modules it may reach: today's set, never"
    print "more. `scripts/dep-budget.sh` runs in the `validate` step and fails when a package reaches a module that is not on"
    print "its lines, naming the module and the go-common package that imports it. Then either:"
    print ""
    print "1. Keep the package small: move the code that needs the module into a package that already depends on it, or into a"
    print "   new one, or drop the import."
    print "2. If the dependency is intended, add the module to the package's line in `scripts/dep-budget.txt`, give the reason in"
    print "   the pull request description, and run `scripts/dep-budget.sh --write-doc` to refresh this page."
    print ""
    print "`scripts/dep-budget.sh --print` prints every package's current set in the budget file's syntax. When a dependency is"
    print "removed, tighten the budget line too: the script notes lines that allow more than the package reaches."
}

# sortkeys sorts keys[1..n] in byte order.
function sortkeys(keys, n,    i, j, t) {
    for (i = 2; i <= n; i++) {
        t = keys[i]
        for (j = i - 1; j >= 1 && keys[j] > t; j--) keys[j + 1] = keys[j]
        keys[j + 1] = t
    }
}
AWK

case "$action" in
  --write-doc)
    analyze doc >"$tmp/doc"
    mv "$tmp/doc" "$doc"
    echo "dep-budget: wrote $doc"
    ;;
  --check-doc)
    analyze doc >"$tmp/doc"
    if ! cmp -s "$tmp/doc" "$doc"; then
      echo "dep-budget: $doc is stale; run scripts/dep-budget.sh --write-doc and commit the result" >&2
      diff -u "$doc" "$tmp/doc" 2>/dev/null | head -n 30 >&2 || true
      exit 1
    fi
    echo "dep-budget: $doc is up to date"
    ;;
  --print)
    analyze print
    ;;
  *)
    analyze check
    ;;
esac
