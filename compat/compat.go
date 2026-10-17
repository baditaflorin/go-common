// Package compat says whether a go-common version is old enough to be missing a
// security fix that go-common itself shipped.
//
// The manifest (compat.json, embedded in the package) names a floor, min_safe,
// and the fixes that put it there. A service pinned below the floor has a known,
// fixed problem; the check is how the fleet turns "please bump go-common" into a
// gate (a preflight or CI step calls CheckGoMod on each service's go.mod).
//
//	m, err := compat.Load()
//	res, err := m.CheckGoMod(goModBytes)
//	if res.Status == compat.StatusBelowFloor { ... }
//
// The floor is about go-common's own fixes. It does not claim that a version at
// or above it is free of known vulnerabilities: advisories in go-common's
// dependencies and in the Go toolchain are tracked in scripts/govulncheck-allow.txt.
package compat

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed compat.json
var embedded []byte

// Reason is one fix that raised the floor.
type Reason struct {
	Version string `json:"version"`
	Summary string `json:"summary"`
}

// Manifest is the parsed compat.json.
type Manifest struct {
	Schema int `json:"schema"`
	// MinSafe is the oldest release that contains every security fix in Reasons.
	MinSafe string `json:"min_safe"`
	// MinSafeDate is when MinSafe was committed, used to place pseudo-versions
	// (which carry only a commit time) relative to the floor.
	MinSafeDate time.Time `json:"min_safe_date"`
	Reasons     []Reason  `json:"reasons"`
	// Lines holds the floor of each older release line that has security
	// backports, keyed "vMAJOR.MINOR" (for example "v0.97": "v0.97.1"). A pin on a
	// listed line is judged against that line's floor, not MinSafe. Empty unless
	// a backport branch has been cut.
	Lines map[string]string `json:"lines,omitempty"`
	Note  string            `json:"note"`
}

// Status is the verdict for one go-common pin.
type Status string

const (
	// StatusOK means the pin is at or above the floor.
	StatusOK Status = "ok"
	// StatusBelowFloor means the pin predates a security fix.
	StatusBelowFloor Status = "below_min_safe"
	// StatusUnchecked means the pin cannot be judged, for example a replace
	// directive pointing at a local checkout.
	StatusUnchecked Status = "unchecked"
	// StatusNotUsed means the go.mod does not require go-common.
	StatusNotUsed Status = "not_used"
)

// Result is the outcome of a check.
type Result struct {
	Status  Status `json:"status"`
	Version string `json:"version,omitempty"`
	Message string `json:"message"`
}

// ModulePath is the module this package is about.
const ModulePath = "github.com/baditaflorin/go-common"

// Load parses the manifest embedded in this version of go-common.
func Load() (Manifest, error) { return Parse(embedded) }

// Parse parses and validates a manifest.
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("compat: parse manifest: %w", err)
	}
	if m.Schema != 1 {
		return Manifest{}, fmt.Errorf("compat: unsupported manifest schema %d", m.Schema)
	}
	if _, err := parseVersion(m.MinSafe); err != nil {
		return Manifest{}, fmt.Errorf("compat: min_safe: %w", err)
	}
	if m.MinSafeDate.IsZero() {
		return Manifest{}, errors.New("compat: min_safe_date is missing")
	}
	for line, floor := range m.Lines {
		fv, err := parseVersion(floor)
		if err != nil {
			return Manifest{}, fmt.Errorf("compat: lines[%q]: %w", line, err)
		}
		if want := fmt.Sprintf("v%d.%d", fv.major, fv.minor); line != want || fv.pre != "" {
			return Manifest{}, fmt.Errorf("compat: lines[%q] = %s must be a release of that line (key %s)", line, floor, want)
		}
	}
	return m, nil
}

type version struct {
	major, minor, patch int
	pre                 string // pre-release part, without the leading '-'
}

var semverRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func parseVersion(s string) (version, error) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return version{}, fmt.Errorf("%q is not a semantic version", s)
	}
	var v version
	v.major, _ = strconv.Atoi(m[1])
	v.minor, _ = strconv.Atoi(m[2])
	v.patch, _ = strconv.Atoi(m[3])
	v.pre = m[4]
	return v, nil
}

// compare orders the numeric core only: -1, 0 or +1.
func (v version) compareCore(o version) int {
	for _, d := range [][2]int{{v.major, o.major}, {v.minor, o.minor}, {v.patch, o.patch}} {
		if d[0] < d[1] {
			return -1
		}
		if d[0] > d[1] {
			return 1
		}
	}
	return 0
}

// A pseudo-version ends in a 14-digit UTC commit time and a 12-digit commit hash.
var pseudoRe = regexp.MustCompile(`(\d{14})-[0-9a-f]{12}$`)

// Check judges one go-common version as written in a go.mod.
func (m Manifest) Check(ver string) (Result, error) {
	v, err := parseVersion(ver)
	if err != nil {
		return Result{}, fmt.Errorf("compat: %w", err)
	}
	floor, err := parseVersion(m.MinSafe)
	if err != nil {
		return Result{}, fmt.Errorf("compat: min_safe: %w", err)
	}

	if p := pseudoRe.FindStringSubmatch(ver); p != nil {
		ts, err := time.Parse("20060102150405", p[1])
		if err != nil {
			return Result{}, fmt.Errorf("compat: pseudo-version %q has an invalid timestamp: %w", ver, err)
		}
		if ts.Before(m.MinSafeDate) {
			return Result{StatusBelowFloor, ver, fmt.Sprintf(
				"%s is a pseudo-version from %s, before %s (%s); pin a release at or above %s",
				ver, ts.Format("2006-01-02"), m.MinSafe, m.MinSafeDate.Format("2006-01-02"), m.MinSafe)}, nil
		}
		return Result{StatusOK, ver, fmt.Sprintf(
			"%s is a pseudo-version committed after %s; judged by commit time, so pin a release when you can", ver, m.MinSafe)}, nil
	}

	// A pin on a release line with backports is judged against that line's floor.
	if lineFloor, ok := m.Lines[fmt.Sprintf("v%d.%d", v.major, v.minor)]; ok {
		lf, err := parseVersion(lineFloor)
		if err != nil {
			return Result{}, fmt.Errorf("compat: lines: %w", err)
		}
		if v.compareCore(lf) < 0 || (v.compareCore(lf) == 0 && v.pre != "") {
			return Result{StatusBelowFloor, ver, fmt.Sprintf("%s is below the floor %s for its release line; %s", ver, lineFloor, m.fixes())}, nil
		}
		return Result{StatusOK, ver, fmt.Sprintf("%s has the backported fixes (floor %s for its line); the newest floor is %s, so plan to move", ver, lineFloor, m.MinSafe)}, nil
	}

	switch c := v.compareCore(floor); {
	case c > 0:
		return Result{StatusOK, ver, fmt.Sprintf("%s is above the floor %s", ver, m.MinSafe)}, nil
	case c == 0 && v.pre == "":
		return Result{StatusOK, ver, fmt.Sprintf("%s is the floor", ver)}, nil
	default: // older core, or a pre-release of the floor itself (which sorts below it)
		return Result{StatusBelowFloor, ver, fmt.Sprintf("%s is below the floor %s; %s", ver, m.MinSafe, m.fixes())}, nil
	}
}

// fixes lists what a version below the floor is missing.
func (m Manifest) fixes() string {
	var parts []string
	for _, r := range m.Reasons {
		parts = append(parts, fmt.Sprintf("%s: %s", r.Version, r.Summary))
	}
	return "it lacks security fixes (" + strings.Join(parts, "; ") + ")"
}

var (
	requireRe = regexp.MustCompile(`^\s*(?:require\s+)?` + regexp.QuoteMeta(ModulePath) + `\s+(v\S+)`)
	replaceRe = regexp.MustCompile(`^\s*(?:replace\s+)?` + regexp.QuoteMeta(ModulePath) + `(?:\s+v\S+)?\s*=>\s*(\S+)`)
)

// CheckGoMod judges the go-common requirement in a go.mod file.
func (m Manifest) CheckGoMod(goMod []byte) (Result, error) {
	var required, replacement string
	for _, line := range strings.Split(string(goMod), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if r := replaceRe.FindStringSubmatch(line); r != nil {
			replacement = r[1]
			continue
		}
		if r := requireRe.FindStringSubmatch(line); r != nil {
			required = r[1]
		}
	}
	switch {
	case replacement != "" && (strings.HasPrefix(replacement, ".") || strings.HasPrefix(replacement, "/")):
		return Result{StatusUnchecked, required, fmt.Sprintf("go-common is replaced by the local path %s; cannot judge it", replacement)}, nil
	case replacement != "":
		return Result{StatusUnchecked, required, fmt.Sprintf("go-common is replaced by %s; cannot judge it", replacement)}, nil
	case required == "":
		return Result{StatusNotUsed, "", "go.mod does not require " + ModulePath}, nil
	}
	return m.Check(required)
}
