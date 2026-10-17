#!/usr/bin/env bash
# govulncheck gate with a reviewed allowlist.
#
#   scripts/vuln-check.sh
#
# Runs `govulncheck ./...` (source mode: only vulnerabilities the code can reach)
# and fails when it reports a reachable vulnerability that is not in
# scripts/govulncheck-allow.txt, or one whose allowlist entry has expired. A
# plain `govulncheck ./...` would be red from day one while a fix is blocked, and
# a gate that is always red is ignored; the allowlist keeps it meaningful.
#
# Run it with a SUPPORTED Go toolchain (CI uses golang:1.26). On an out-of-support
# one, standard-library findings with no fix drown out the module findings that
# are go-common's own to fix. VULN_ALLOWLIST overrides the allowlist path.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=scripts/tool-versions.sh
source scripts/tool-versions.sh
allow_file="${VULN_ALLOWLIST:-scripts/govulncheck-allow.txt}"

if ! command -v govulncheck >/dev/null 2>&1; then
  echo "vuln-check: installing govulncheck@${GOVULNCHECK_VERSION}"
  go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"
  PATH="$(go env GOPATH)/bin:$PATH"
fi

out="$(mktemp)"
trap 'rm -f "$out"' EXIT
rc=0
govulncheck ./... >"$out" 2>&1 || rc=$?
cat "$out"
# 0: clean. 3: vulnerabilities found. Anything else: the tool itself failed.
if [ "$rc" -ne 0 ] && [ "$rc" -ne 3 ]; then
  echo "vuln-check: govulncheck failed (exit $rc), not a scan result" >&2
  exit 1
fi

# "Vulnerability #N: GO-YYYY-NNNN" appears only for reachable vulnerabilities.
found="$(grep -oE '^Vulnerability #[0-9]+: GO-[0-9]+-[0-9]+' "$out" | grep -oE 'GO-[0-9]+-[0-9]+' | sort -u || true)"

declare -A expires=()
if [ -f "$allow_file" ]; then
  while read -r id date _; do
    case "$id" in ''|'#'*) continue ;; esac
    expires["$id"]="$date"
  done <"$allow_file"
fi

today="$(date -u +%F)"
fail=0
echo
for id in $found; do
  if [ -z "${expires[$id]:-}" ]; then
    echo "vuln-check: NEW reachable vulnerability $id (https://pkg.go.dev/vuln/$id)" >&2
    fail=1
  elif [[ "$today" > "${expires[$id]}" ]]; then
    echo "vuln-check: allowlist entry for $id expired on ${expires[$id]}; fix it or renew it with a reason" >&2
    fail=1
  else
    echo "vuln-check: $id is allowlisted until ${expires[$id]}"
  fi
done
for id in "${!expires[@]}"; do
  if ! grep -qx "$id" <<<"$found"; then
    echo "vuln-check: warning: $id is allowlisted but no longer reported; remove it from $allow_file"
  fi
done

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "vuln-check: no reachable vulnerability outside the allowlist"
