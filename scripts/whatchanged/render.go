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
func render(from, to string, changes []compat.Change) string {
	var b strings.Builder
	if to == "" {
		fmt.Fprintf(&b, "### go-common changes to check since `%s`\n\n", from)
	} else {
		fmt.Fprintf(&b, "### go-common changes to check between `%s` and `%s`\n\n", from, to)
	}
	if len(changes) == 0 {
		b.WriteString("None: no CHANGELOG.md entry in that range is marked BREAKING or BEHAVIOURAL.\n")
		return b.String()
	}
	b.WriteString("The CHANGELOG.md bullets marked BREAKING or BEHAVIOURAL in that range, one line each; the entry has the full text and the reason.\n\n")
	for _, c := range changes {
		fmt.Fprintf(&b, "- **%s** (%s), %s: %s\n", c.Version, c.Date, label(c), c.Summary)
		if c.OffSwitch != "" {
			fmt.Fprintf(&b, "  Off-switch: %s\n", c.OffSwitch)
		}
		if c.Reason != "" {
			fmt.Fprintf(&b, "  Why a patch release carries it: %s\n", c.Reason)
		}
	}
	return b.String()
}
