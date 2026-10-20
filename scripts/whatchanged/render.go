package main

import (
	"fmt"
	"strings"

	"github.com/baditaflorin/go-common/compat"
)

// label is a change's marker as it is written in CHANGELOG.md.
func label(c compat.Change) string {
	l := strings.ToUpper(c.Class)
	if c.Qualifier != "" {
		l += " (" + c.Qualifier + ")"
	}
	return l
}

// render is the markdown section for a pull request body: what changes for a service
// that moves from `from` to `to` (the newest release when to is empty).
func render(from, to string, changes []compat.Change, m compat.Manifest) string {
	var b strings.Builder
	if to == "" {
		fmt.Fprintf(&b, "### go-common changes to check since `%s`\n\n", from)
	} else {
		fmt.Fprintf(&b, "### go-common changes to check between `%s` and `%s`\n\n", from, to)
	}
	if len(changes) == 0 {
		b.WriteString("None: no CHANGELOG.md entry in that range is marked BREAKING, BEHAVIOURAL or SECURITY.\n")
		return b.String()
	}
	b.WriteString("The CHANGELOG.md bullets marked BREAKING, BEHAVIOURAL or SECURITY in that range, one line each; the entry has the full text and the reason.\n\n")
	for _, c := range changes {
		fmt.Fprintf(&b, "- **%s** (%s), %s: %s\n", c.Version, c.Date, label(c), c.Summary)
		if c.OffSwitch != "" {
			fmt.Fprintf(&b, "  Off-switch: %s\n", c.OffSwitch)
		}
		if c.Class == "security" {
			fmt.Fprintf(&b, "  Floor: %s\n", floorLine(m, c.Version))
		}
		if c.Reason != "" {
			fmt.Fprintf(&b, "  Why a patch release carries it: %s\n", c.Reason)
		}
	}
	return b.String()
}

// floorLine says what compat/compat.json does about the fix a SECURITY bullet describes.
func floorLine(m compat.Manifest, version string) string {
	status, reason, err := m.FloorDecision(version)
	switch {
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case status == compat.FloorCovers:
		return "min_safe is " + m.MinSafe + ", at or above this release, so a pin below it is reported by compatcheck"
	case status == compat.FloorUnchanged:
		return "min_safe is still " + m.MinSafe + ", on purpose: " + reason
	}
	return "min_safe is " + m.MinSafe + " and no decision is recorded for this release"
}
