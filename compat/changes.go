package compat

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"time"
)

// changes.json is generated from CHANGELOG.md by `go run ./scripts/whatchanged -gen`
// and embedded here, so `go run .../scripts/whatchanged@latest <from> <to>` and any
// preflight or dashboard that imports this package see the same facts.
//
//go:embed changes.json
var embeddedChanges []byte

// Change is one CHANGELOG.md bullet that a consumer has to read before it bumps:
// a bullet whose marker is **BREAKING...**, written with a one-line summary and,
// when there is one, the way back to the old behaviour (docs/releasing.md has the
// grammar).
type Change struct {
	// Version is the release that shipped it, for example "v0.103.0".
	Version string `json:"version"`
	// Date is the release date in the CHANGELOG heading, "YYYY-MM-DD".
	Date string `json:"date"`
	// Class is the bullet's marker in lower case: "breaking".
	Class string `json:"class"`
	// Qualifier is the text in parentheses after the marker, for example
	// "behavioural" or "only for 0.102.59"; empty when there is none, which for
	// a breaking change means the API itself (or a build requirement) changed.
	Qualifier string `json:"qualifier,omitempty"`
	// Summary is the bullet's summary: line, at most 200 characters.
	Summary string `json:"summary"`
	// OffSwitch is the bullet's off-switch: line, the setting, option or call that
	// restores the old behaviour without waiting for a release. Empty when there is none.
	OffSwitch string `json:"off_switch,omitempty"`
}

// ChangeFeed is the parsed changes.json, newest release first.
type ChangeFeed struct {
	Schema  int      `json:"schema"`
	Note    string   `json:"note,omitempty"`
	Changes []Change `json:"changes"`
}

// LoadChanges parses the feed embedded in this version of go-common.
func LoadChanges() (ChangeFeed, error) { return ParseChanges(embeddedChanges) }

// ParseChanges parses and validates a feed. It checks what a consumer relies on
// (the schema, a version and a date on every change) and ignores fields and
// classes it does not know, so an older consumer can read a newer feed.
func ParseChanges(data []byte) (ChangeFeed, error) {
	var f ChangeFeed
	if err := json.Unmarshal(data, &f); err != nil {
		return ChangeFeed{}, fmt.Errorf("compat: parse change feed: %w", err)
	}
	if f.Schema != 1 {
		return ChangeFeed{}, fmt.Errorf("compat: unsupported change feed schema %d", f.Schema)
	}
	for i, c := range f.Changes {
		v, err := parseVersion(c.Version)
		if err != nil {
			return ChangeFeed{}, fmt.Errorf("compat: changes[%d]: %w", i, err)
		}
		if v.pre != "" {
			return ChangeFeed{}, fmt.Errorf("compat: changes[%d]: %q must be a release", i, c.Version)
		}
		if _, err := time.Parse(time.DateOnly, c.Date); err != nil {
			return ChangeFeed{}, fmt.Errorf("compat: changes[%d] (%s): date %q is not YYYY-MM-DD", i, c.Version, c.Date)
		}
	}
	return f, nil
}

// Between returns the changes shipped in releases newer than from, up to and
// including to, in the feed's order (newest first). It answers "what changes if I
// move my pin from from to to". Either end may be empty: from "" starts at the
// first release in the feed, to "" has no upper end.
//
// A pin that is a pre-release or a pseudo-version sorts below its own release
// (v0.102.3-0.20261002000000-abcdefabcdef is older than v0.102.3), so the
// release it was cut for is included: a note that might not apply is better than
// one that is missing.
func (f ChangeFeed) Between(from, to string) ([]Change, error) {
	var lo, hi *version
	if from != "" {
		v, err := parseVersion(from)
		if err != nil {
			return nil, fmt.Errorf("compat: from: %w", err)
		}
		lo = &v
	}
	if to != "" {
		v, err := parseVersion(to)
		if err != nil {
			return nil, fmt.Errorf("compat: to: %w", err)
		}
		hi = &v
	}
	if lo != nil && hi != nil && lo.compareCore(*hi) > 0 {
		return nil, fmt.Errorf("compat: from %s is newer than to %s", from, to)
	}
	var out []Change
	for _, c := range f.Changes {
		v, err := parseVersion(c.Version)
		if err != nil {
			return nil, fmt.Errorf("compat: %w", err)
		}
		if lo != nil && !releaseNewerThan(v, *lo) {
			continue
		}
		if hi != nil && releaseNewerThan(v, *hi) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// releaseNewerThan reports whether the release r sorts after o. A pre-release of
// the same numbers sorts below the release.
func releaseNewerThan(r, o version) bool {
	if c := r.compareCore(o); c != 0 {
		return c > 0
	}
	return o.pre != ""
}
