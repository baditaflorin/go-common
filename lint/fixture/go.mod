// A nested module on purpose: `go build ./...` and golangci-lint at the repo
// root skip it. lint/selftest.sh lints it with lint/golangci.yml and checks the
// shared rules fire.
module example.com/lintfixture

go 1.25
