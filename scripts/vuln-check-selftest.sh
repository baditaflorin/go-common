#!/usr/bin/env bash
# Verify verdicts with a fake govulncheck; no Go toolchain or network required.
set -euo pipefail

root="$(cd "${BASH_SOURCE[0]%/*}/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/repo/scripts"
cp "$root/scripts/vuln-check.sh" "$root/scripts/tool-versions.sh" "$tmp/repo/scripts/"
cat >"$tmp/bin/govulncheck" <<'FAKE'
#!/usr/bin/env bash
cat "$FAKE/output"
exit "$FAKE_RC"
FAKE
chmod +x "$tmp/bin/govulncheck" "$tmp/repo/scripts/vuln-check.sh"
export PATH="$tmp/bin:$PATH" FAKE="$tmp" VULN_ALLOWLIST="$tmp/allow"
: >"$tmp/allow"
run() { (cd "$tmp/repo" && scripts/vuln-check.sh) 2>&1; }
fail() { echo "vuln-check-selftest: FAIL: $*" >&2; exit 1; }
passes() { local out; out="$(run)" || fail "$1: $out"; }
rejects() {
  local out
  if out="$(run)"; then fail "$1 passed: $out"; fi
  [[ "$out" == *"$2"* ]] || fail "$1 had no expected verdict '$2': $out"
}

export FAKE_RC=0
printf 'No vulnerabilities found.\n' >"$tmp/output"
passes "a clean scan"
export FAKE_RC=3
printf 'The output format changed: GO-2099-1234\n' >"$tmp/output"
rejects "changed finding format" 'no vulnerability IDs could be parsed'
: >"$tmp/output"
rejects "empty output with vulnerability exit status" 'no vulnerability IDs could be parsed'
printf 'Vulnerability #1: GO-2099-1234\n' >"$tmp/output"
rejects "a new reachable vulnerability" 'NEW reachable vulnerability GO-2099-1234'
printf 'GO-2099-1234 2999-12-31 fixture\n' >"$tmp/allow"
passes "a reviewed unexpired finding"
printf 'GO-2099-1234 2000-01-01 fixture\n' >"$tmp/allow"
rejects "an expired finding" 'expired on 2000-01-01'
printf 'GO-2099-1234 2999-12-31 fixture\n' >"$tmp/allow"
printf 'Vulnerability #2: GO-2099-5678\n' >>"$tmp/output"
rejects "one allowed finding cannot hide another" 'NEW reachable vulnerability GO-2099-5678'
export FAKE_RC=2
rejects "a scanner execution failure" 'govulncheck failed (exit 2)'

echo "vuln-check-selftest: ok"
