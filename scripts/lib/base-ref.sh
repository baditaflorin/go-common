# Sourced by the gate scripts (apidiff-check.sh, bench-gate.sh). Not executable.
#
# resolve_base_commit [ref] prints the commit a change should be compared with,
# or prints a reason on stderr and returns 1. The base is the first of:
#   1. the argument, or $BASE_REF, used exactly as given
#   2. in a Woodpecker pull request, the MERGE BASE of HEAD and
#      origin/$CI_COMMIT_TARGET_BRANCH. The target branch tip is wrong here: it
#      may have moved on since the PR branched, and everything merged meanwhile
#      would then look like something the PR removed or regressed.
#   3. in CI on a push, HEAD^ (changes are squash-merged, so that is the previous
#      state of the branch)
#   4. the newest v* tag
#
# A shallow CI clone may lack the base. It is fetched (and the history unshallowed)
# before giving up: a gate that silently skips would be worse than a failed run
# that can be retried.
# has_parent_commit reports whether HEAD^ exists, deepening a shallow clone by one
# commit first (Woodpecker's default clone has depth 1).
has_parent_commit() {
  if ! git rev-parse -q --verify 'HEAD^' >/dev/null 2>&1 && [ "$(git rev-parse --is-shallow-repository)" = "true" ]; then
    git fetch -q --no-tags --deepen=1 origin 2>/dev/null || true
  fi
  git rev-parse -q --verify 'HEAD^' >/dev/null 2>&1
}

resolve_base_commit() {
  local arg="${1:-}" ref="" pr_mode=0

  if [ -n "$arg" ]; then
    ref="$arg"
  elif [ -n "${BASE_REF:-}" ]; then
    ref="$BASE_REF"
  elif [ -n "${CI_COMMIT_TARGET_BRANCH:-}" ] && [ "${CI_PIPELINE_EVENT:-}" = "pull_request" ]; then
    ref="origin/${CI_COMMIT_TARGET_BRANCH}"
    pr_mode=1
  elif [ -n "${CI:-}" ] && has_parent_commit; then
    ref="HEAD^"
  else
    ref="$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
  fi
  if [ -z "$ref" ]; then
    echo "no base ref (pass one, set BASE_REF, or fetch tags)" >&2
    return 1
  fi

  if ! git rev-parse -q --verify "${ref}^{commit}" >/dev/null 2>&1; then
    git fetch -q --no-tags origin "${ref#origin/}" 2>/dev/null || true
    if [ "$(git rev-parse --is-shallow-repository)" = "true" ]; then
      git fetch -q --no-tags --unshallow origin 2>/dev/null || true
    fi
    if ! git rev-parse -q --verify "${ref}^{commit}" >/dev/null 2>&1; then
      echo "cannot resolve base ref '${ref}'" >&2
      return 1
    fi
  fi

  if [ "$pr_mode" = 1 ]; then
    local mb
    mb="$(git merge-base HEAD "$ref" 2>/dev/null || true)"
    if [ -z "$mb" ] && [ "$(git rev-parse --is-shallow-repository)" = "true" ]; then
      git fetch -q --no-tags --unshallow origin 2>/dev/null || true
      mb="$(git merge-base HEAD "$ref" 2>/dev/null || true)"
    fi
    if [ -z "$mb" ]; then
      echo "no merge base between HEAD and ${ref}" >&2
      return 1
    fi
    ref="$mb"
  fi

  git rev-parse "${ref}^{commit}"
}
