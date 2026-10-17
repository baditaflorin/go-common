package graph

import (
	"log"
	"strings"
	"sync"
	"sync/atomic"
)

// forbiddenServices are the services that must never run the Fleet Graph writer. The names are the fleet's canonical registry ids; the
// repository, compose and container names of the same service ("go-x", "go_x") normalise to them. The list is deliberately short and
// pinned by a test: go_fleet_runner (graph_writer_forbidden.go, which refuses an enabled manifest row and renders GRAPH_ENABLED=false for
// them) and services-registry (bin/test_graph_forbidden.py) hold the same four names, and docs/graph-writer-targets.md in go_fleet_runner
// has the reasons. This file is the last layer: it keeps a process from emitting even if every control-plane check were bypassed (a hand
// edited compose file, a container started by hand with GRAPH_ENABLED=true and a key mounted).
//
//	fleet-graph     the collector: the sink; its own ingest is skipped in both directions already, but it learns no edge that its callers'
//	                outbound events do not give, and every extra event is a write on its single SQLite connection.
//	apikey-service  the keystore: the collector authenticates every writer batch through it, so keystore telemetry closes
//	                keystore -> collector -> keystore, and tier 0 must not depend on the collector.
//	fleet-secrets   the vault front: writer keys are materialised from it (a bootstrap cycle during a vault failover).
//	keystore-mesh   the keystore nodes on other hosts.
var forbiddenServices = []string{"fleet-graph", "apikey-service", "fleet-secrets", "keystore-mesh"}

var (
	// forbiddenIdentitySeen is sticky: once this process has been identified as a forbidden service it stays forbidden, whatever a later
	// Init is given.
	forbiddenIdentitySeen    atomic.Bool
	forbiddenIdentityLogOnce sync.Once
)

// forbiddenKey normalises the names one service goes by (registry id, "go-x", "go_x") to one key.
func forbiddenKey(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "_", "-")
	return strings.TrimPrefix(name, "go-")
}

// identityForbidden reports whether any of the names is a service that must never run the writer.
func identityForbidden(names ...string) bool {
	for _, name := range names {
		key := forbiddenKey(name)
		if key == "" {
			continue
		}
		for _, f := range forbiddenServices {
			if key == f {
				return true
			}
		}
	}
	return false
}

// processForbidden decides, from the identity a caller passes to Init and the GRAPH_SERVICE_ID in the environment, whether this process
// is a forbidden service, and remembers the answer.
func processForbidden(passedServiceID string) bool {
	if forbiddenIdentitySeen.Load() {
		return true
	}
	if identityForbidden(passedServiceID, envServiceID.Value()) {
		forbiddenIdentitySeen.Store(true)
		return true
	}
	return false
}

// warnForbiddenIdentity logs, once per process, that Graph emission is off because this service must never run the writer. Like
// warnInvalidServiceID it stays silent when Graph is not configured to emit anyway (no flag or no key: nothing changes), and it names no
// key, secret or value.
func warnForbiddenIdentity(cfg config) {
	if !cfg.eventEmissionConfigured() {
		return
	}
	forbiddenIdentityLogOnce.Do(func() {
		log.Printf("graph: this service must never run the Fleet Graph writer (the collector, the keystore, the vault front and the keystore mesh: " +
			"a feedback cycle through the collector's own authentication, and tier-0 services must not depend on the collector): Fleet Graph " +
			"event emission is OFF for this process and every event is dropped (counted in fleet_graph_events_dropped_invalid_identity_total); " +
			"remove GRAPH_ENABLED from its environment")
	})
}
