//go:build !race

package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestReleaseCheckSelftest runs scripts/release-check-selftest.sh, so that the validate
// step tests release-check.sh in every pipeline, as it does the other scripts that have a
// selftest. It is left out of the race build: the script compiles and runs this package in
// throwaway repositories, which is not what -race is for.
func TestReleaseCheckSelftest(t *testing.T) {
	for _, tool := range []string{"bash", "git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	out, err := exec.Command("bash", filepath.Join(moduleRoot, "scripts", "release-check-selftest.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("scripts/release-check-selftest.sh: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
