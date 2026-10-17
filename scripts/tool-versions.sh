# Pinned versions of the tools CI and the helper scripts install. Sourced, not
# executed. Bump deliberately, prefer releases that are at least a few days old,
# and never install a tool with @latest in CI (supply chain).
#
# golangci-lint is run from its container image in .woodpecker.yml; keep the
# image tag there equal to GOLANGCI_LINT_VERSION.
GOLANGCI_LINT_VERSION=v2.14.0
# govulncheck v1.8.0 and later need Go >= 1.26, so the vuln step runs on a
# golang:1.26 image (see scripts/vuln-check.sh); the rest of CI builds with 1.25.
GOVULNCHECK_VERSION=v1.8.0
# apidiff is not tagged; this is a golang.org/x/exp commit (2026-07-27).
APIDIFF_VERSION=fd0e80228fd8
