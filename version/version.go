// Package version exposes build identity variables that are injected at
// link time via -ldflags. Using ldflags guarantees the binary always
// carries its true version regardless of whether service.yaml is present.
//
// Canonical build command:
//
//	go build \
//	  -ldflags "-X github.com/baditaflorin/go-common/version.Tag=$(git describe --tags --always) \
//	             -X github.com/baditaflorin/go-common/version.GitCommit=$(git rev-parse --short HEAD) \
//	             -X github.com/baditaflorin/go-common/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
//	  ./...
//
// In development (no ldflags), all variables default to their zero-value
// sentinels ("dev", "HEAD", "unknown") so logs are always unambiguous.
//
// Usage:
//
//	import "github.com/baditaflorin/go-common/version"
//
//	fmt.Println(version.Tag)         // "v1.4.2"
//	fmt.Println(version.GitCommit)   // "a3f9b1c"
//	fmt.Println(version.BuildDate)   // "2026-05-19T14:23:00Z"
//	fmt.Println(version.String())    // "v1.4.2 (a3f9b1c, built 2026-05-19T14:23:00Z)"
package version

import "fmt"

// Tag is the git tag or semver string, e.g. "v1.4.2".
// Injected via: -ldflags "-X github.com/baditaflorin/go-common/version.Tag=v1.4.2"
var Tag = "dev"

// GitCommit is the short git commit SHA, e.g. "a3f9b1c".
// Injected via: -ldflags "-X github.com/baditaflorin/go-common/version.GitCommit=a3f9b1c"
var GitCommit = "HEAD"

// BuildDate is the UTC build timestamp in RFC3339 format.
// Injected via: -ldflags "-X github.com/baditaflorin/go-common/version.BuildDate=2026-05-19T14:23:00Z"
var BuildDate = "unknown"

// GoVersion is the Go toolchain version used to build the binary.
// This is populated automatically at runtime via runtime.Version()
// and does not need ldflags injection.
var GoVersion = goVersion()

// String returns a single-line human-readable version string suitable
// for /version responses, log startup lines, and User-Agent headers.
func String() string {
	return fmt.Sprintf("%s (%s, built %s)", Tag, GitCommit, BuildDate)
}

// Info returns a structured map suitable for JSON serialisation.
// Use in /version endpoints alongside config.Load() fields.
func Info() map[string]string {
	return map[string]string{
		"tag":        Tag,
		"git_commit": GitCommit,
		"build_date": BuildDate,
		"go_version": GoVersion,
	}
}
