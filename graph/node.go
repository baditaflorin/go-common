package graph

import (
	"log"
	"os"
	"strings"
	"sync"
)

// NodeHeader is the HTTP header a Graph writer puts on every batch POST to say which
// logical host (the VM the process runs on) the batch comes from. It is a header and
// not a JSON field on purpose: the collector decodes batches with DisallowUnknownFields,
// so a new field would make an older collector reject every batch from a newer sender,
// while an unknown header is simply ignored. The value is self-asserted diagnostic
// metadata, never an authorization input.
const NodeHeader = "X-Fleet-Node"

var nodeInvalidLogOnce sync.Once

// loadNode reads FLEET_NODE: the logical host name of the VM this process runs on, as
// named in the fleet host-placement policy ("dockerhost", "infer-runtime", ...). It is
// injected per host (and per pool replica) by the deploy tooling. The value must be a
// lowercase DNS label, like a service id; anything else is ignored with one log line so
// a typo cannot put arbitrary text into a request header.
func loadNode() string {
	raw := strings.TrimSpace(os.Getenv("FLEET_NODE"))
	if raw == "" {
		return ""
	}
	node := strings.ToLower(raw)
	if !validGraphLabel(node) {
		nodeInvalidLogOnce.Do(func() {
			log.Printf("graph: FLEET_NODE is set but is not a valid lowercase DNS label " +
				"([a-z0-9-], at most 63 characters, no leading or trailing '-'): it is ignored and batches carry no node")
		})
		return ""
	}
	return node
}

// Node returns the logical host this process reports in the X-Fleet-Node header of its
// batches, or "" when FLEET_NODE is unset or invalid. It is read once, at first use.
func Node() string {
	return ensureInit().cfg.node
}
