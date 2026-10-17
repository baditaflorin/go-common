#!/usr/bin/env bash
# Checks that lint/golangci.yml still fires on lint/fixture, the deliberately bad
# nested module, and stays quiet on the fixture's clean function.
#
#   lint/selftest.sh            # needs golangci-lint v2 on PATH
#
# Each `// want: <linter>` marker at the end of a line in lint/fixture/main.go
# must produce a finding from that linter on that line (more findings on a marked
# line are fine, e.g. noctx also flags http.Get). A finding on a line without a
# marker is a failure: that is how the clean function is held to account.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/fixture"
config="../golangci.yml"
src="main.go"

out="$(golangci-lint run --config "$config" --max-issues-per-linter 0 --max-same-issues 0 ./... 2>&1 || true)"

# Finding lines look like "fixture/main.go:16:23: message (linter)"; the path
# prefix depends on where golangci-lint is run from, so match on the file name.
findings="$(printf '%s\n' "$out" | grep -E "(^|/)${src//./\\.}:[0-9]+:[0-9]+: " || true)"

fail=0
declare -A expected=()
while IFS=: read -r lineno rest; do
  expected["$lineno"]="$(printf '%s' "$rest" | sed -E 's/.*\/\/ want: *([A-Za-z0-9]+)[[:space:]]*$/\1/')"
done < <(grep -nE '// want: [A-Za-z0-9]+[[:space:]]*$' "$src")

if [ "${#expected[@]}" -eq 0 ]; then
  echo "selftest: no '// want:' markers found in ${src}" >&2
  exit 1
fi

for lineno in "${!expected[@]}"; do
  linter="${expected[$lineno]}"
  on_line="$(printf '%s\n' "$findings" | grep -E "(^|/)${src//./\\.}:${lineno}:[0-9]+: " || true)"
  if [ "$linter" = "SA1019" ]; then
    hit="$(printf '%s\n' "$on_line" | grep -E 'SA1019' || true)"
  else
    hit="$(printf '%s\n' "$on_line" | grep -E "\\(${linter}\\)\$" || true)"
  fi
  if [ -z "$hit" ]; then
    echo "MISSING: ${src}:${lineno} should be reported by ${linter}" >&2
    fail=1
  fi
done

while IFS= read -r finding; do
  [ -z "$finding" ] && continue
  lineno="$(printf '%s' "$finding" | sed -E "s/^.*${src//./\\.}:([0-9]+):[0-9]+: .*/\\1/")"
  if [ -z "${expected[$lineno]:-}" ]; then
    echo "UNEXPECTED: $finding" >&2
    fail=1
  fi
done <<<"$findings"

if [ "$fail" -ne 0 ]; then
  echo "--- golangci-lint output ---" >&2
  printf '%s\n' "$out" >&2
  exit 1
fi
echo "lint selftest ok: ${#expected[@]} expected findings, none unexpected"
