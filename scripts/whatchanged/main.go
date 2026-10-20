// Command whatchanged prints the behaviour changes between two go-common releases
// and maintains the feed they are read from.
//
//	go run github.com/baditaflorin/go-common/scripts/whatchanged@latest v0.102.60 v0.105.0
//
// prints a markdown section for a pull request body: the CHANGELOG.md bullets marked
// **BREAKING...** in releases newer than the first version, up to and including the
// second (the newest when it is left out), each as its one-line summary: and, when
// it has one, the off-switch: that restores the old behaviour. Running it with @latest
// reads the newest feed, which is the point: the feed is embedded in the compat package.
//
// The feed is compat/changes.json, generated from CHANGELOG.md:
//
//	go run ./scripts/whatchanged -gen             write compat/changes.json
//	go run ./scripts/whatchanged -check           exit 1 if CHANGELOG.md breaks the grammar or the file is stale
//	go run ./scripts/whatchanged -release 0.105.1 -latest-tag v0.105.0   the checks a change that claims that release must pass
//
// docs/releasing.md has the grammar. scripts/release-check.sh runs -release in CI.
// Exit status: 0 ok, 1 a problem was found (printed), 2 it could not run.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/baditaflorin/go-common/compat"
)

const (
	changelogPath = "CHANGELOG.md"
	feedPath      = "compat/changes.json"
	manifestPath  = "compat/compat.json"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whatchanged", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "module root, for -gen, -check and -release")
	gen := fs.Bool("gen", false, "write "+feedPath+" from "+changelogPath)
	check := fs.Bool("check", false, "fail if "+changelogPath+" breaks the grammar or "+feedPath+" is stale")
	release := fs.String("release", "", "check the entry of this release (for example 0.105.1) as a change that claims it must pass")
	latestTag := fs.String("latest-tag", "", "with -release: the newest tag, for the rule that a behaviour change takes a minor release")
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: whatchanged <from> [<to>]
       whatchanged -gen | -check | -release X.Y.Z [-root dir]

Prints the CHANGELOG.md bullets marked BREAKING in releases newer than <from>, up to
and including <to> (default: the newest), as markdown for a pull request body.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	modes := 0
	for _, on := range []bool{*gen, *check, *release != ""} {
		if on {
			modes++
		}
	}
	switch {
	case modes > 1:
		fmt.Fprintln(stderr, "whatchanged: -gen, -check and -release are separate modes")
		return 2
	case *gen:
		return runGen(*root, stdout, stderr)
	case *check:
		return runCheck(*root, stdout, stderr)
	case *release != "":
		return runRelease(*root, normalize(*release), normalize(*latestTag), stdout, stderr)
	}
	return runPrint(fs.Args(), stdout, stderr)
}

// normalize accepts a version with or without the leading v.
func normalize(v string) string {
	v = strings.TrimSpace(v)
	if v != "" && v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

func runPrint(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(stderr, "whatchanged: give the version you are on and, optionally, the one you move to (whatchanged -h)")
		return 2
	}
	from, to := normalize(args[0]), ""
	if len(args) == 2 {
		to = normalize(args[1])
	}
	feed, err := compat.LoadChanges()
	if err != nil {
		fmt.Fprintln(stderr, "whatchanged:", err)
		return 2
	}
	manifest, err := compat.Load()
	if err != nil {
		fmt.Fprintln(stderr, "whatchanged:", err)
		return 2
	}
	changes, err := feed.Between(from, to)
	if err != nil {
		fmt.Fprintln(stderr, "whatchanged:", err)
		return 2
	}
	fmt.Fprint(stdout, render(from, to, changes, manifest))
	return 0
}

// source is CHANGELOG.md as parsed, and the feed it says.
type source struct {
	entries []*entry
	feed    compat.ChangeFeed
	json    []byte // the feed as compat/changes.json holds it
	probs   []problem
}

func load(root string) (*source, error) {
	src, err := os.ReadFile(filepath.Join(root, changelogPath))
	if err != nil {
		return nil, err
	}
	s := &source{}
	s.entries, s.probs = parseChangelog(src)
	s.feed = feedOf(s.entries)
	if s.json, err = marshalFeed(s.feed); err != nil {
		return nil, err
	}
	return s, nil
}

// marshalFeed is compat/changes.json as it is committed: indented, with the characters
// of the summaries left as they are (no \u003c for a <) and a newline at the end.
func marshalFeed(f compat.ChangeFeed) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func report(w io.Writer, probs []problem) {
	for _, p := range probs {
		fmt.Fprintf(w, "whatchanged: %s\n", p)
	}
}

func runGen(root string, stdout, stderr io.Writer) int {
	s, err := load(root)
	if err != nil {
		fmt.Fprintln(stderr, "whatchanged:", err)
		return 2
	}
	if len(s.probs) > 0 {
		report(stderr, s.probs)
		return 1
	}
	if err := os.WriteFile(filepath.Join(root, feedPath), s.json, 0o644); err != nil {
		fmt.Fprintln(stderr, "whatchanged:", err)
		return 2
	}
	fmt.Fprintf(stdout, "whatchanged: wrote %s (%d changes)\n", feedPath, len(s.feed.Changes))
	return 0
}

// verify is the part of -check and -release that does not depend on a release: the
// grammar holds everywhere and the committed feed is what CHANGELOG.md says.
func verify(root string) (*source, []problem, error) {
	s, err := load(root)
	if err != nil {
		return nil, nil, err
	}
	probs := s.probs
	if len(probs) > 0 {
		return s, probs, nil
	}
	got, err := os.ReadFile(filepath.Join(root, feedPath))
	switch {
	case os.IsNotExist(err):
		probs = append(probs, problem{0, feedPath + " is missing; run `go run ./scripts/whatchanged -gen` and commit it"})
	case err != nil:
		return nil, nil, err
	case !bytes.Equal(got, s.json):
		probs = append(probs, problem{0, feedPath + " is stale: it does not say what " + changelogPath + " says; run `go run ./scripts/whatchanged -gen` and commit the result"})
	}
	return s, probs, nil
}

func runCheck(root string, stdout, stderr io.Writer) int {
	s, probs, err := verify(root)
	if err != nil {
		fmt.Fprintln(stderr, "whatchanged:", err)
		return 2
	}
	if len(probs) > 0 {
		report(stderr, probs)
		return 1
	}
	fmt.Fprintf(stdout, "whatchanged: %s is current (%d changes)\n", feedPath, len(s.feed.Changes))
	return 0
}

func runRelease(root, version, latestTag string, stdout, stderr io.Writer) int {
	s, probs, err := verify(root)
	if err != nil {
		fmt.Fprintln(stderr, "whatchanged:", err)
		return 2
	}
	var claimed *entry
	for _, e := range s.entries {
		if e.version == version {
			claimed = e
		}
	}
	if claimed == nil {
		probs = append(probs, problem{0, fmt.Sprintf("%s has no entry for %s", changelogPath, version)})
	} else if len(probs) == 0 {
		probs = append(probs, releaseProblems(claimed, latestTag)...)
		var m *compat.Manifest
		data, err := os.ReadFile(filepath.Join(root, manifestPath))
		if err == nil {
			var parsed compat.Manifest
			if parsed, err = compat.Parse(data); err == nil {
				m = &parsed
			}
		}
		probs = append(probs, securityProblems(claimed, m, err)...)
	}
	if len(probs) > 0 {
		report(stderr, probs)
		return 1
	}
	fmt.Fprintf(stdout, "whatchanged: %s: %d feed bullets, %s is current\n", version, len(claimed.bullets), feedPath)
	return 0
}
