package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/compat"
)

// moduleRoot is the repository these tests guard: this package is scripts/whatchanged.
const moduleRoot = "../.."

// TestFeedIsCurrent is the drift check for compat/changes.json, in the way
// `go run ./scripts/pkgindex -check` is one for docs/PACKAGES.md: it runs in the
// validate step of every pipeline, so a change that adds or edits a BREAKING bullet
// cannot merge without the fields and the regenerated file.
func TestFeedIsCurrent(t *testing.T) {
	t.Parallel()
	_, probs, err := verify(moduleRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probs {
		t.Error(p)
	}
}

// TestEveryBreakingBulletIsInTheFeed counts the bullets with a deliberately dumb scan
// of CHANGELOG.md, which does not share the parser's rules, so that a parser that
// quietly skipped a bullet cannot make the feed look complete.
func TestEveryBreakingBulletIsInTheFeed(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join(moduleRoot, changelogPath))
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "- **BREAKING") {
			want++
		}
	}
	feed, err := compat.LoadChanges()
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, c := range feed.Changes {
		if c.Class == "breaking" {
			got++
		}
	}
	if want == 0 || got != want {
		t.Fatalf("CHANGELOG.md has %d bullets that start with **BREAKING, the embedded feed holds %d", want, got)
	}
}
