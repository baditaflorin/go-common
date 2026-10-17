package graph

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// configureWriter gives the process a complete writer configuration (flag, collector, key) pointed at a loopback collector that counts what
// it receives, so a test can tell "dropped on purpose" from "not configured".
func configureWriter(t *testing.T) *int64 {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GRAPH_ENABLED", "true")
	t.Setenv("GRAPH_COLLECTOR_URL", srv.URL)
	t.Setenv("GRAPH_FLUSH_INTERVAL", "1")
	t.Setenv("GRAPH_API_KEY", "test-key")
	t.Setenv("GRAPH_SERVICE_ID", "")
	return &hits
}

func recordFive() {
	for i := 0; i < 5; i++ {
		Record(Event{Direction: "in", Path: "/x", Method: "GET", Status: 200, LatencyMs: 3})
	}
}

// A change to the list is a decision: go_fleet_runner (graph_writer_forbidden.go) and services-registry (bin/test_graph_forbidden.py) pin the
// same four names.
func TestForbiddenServicesArePinned(t *testing.T) {
	want := []string{"fleet-graph", "apikey-service", "fleet-secrets", "keystore-mesh"}
	if !reflect.DeepEqual(forbiddenServices, want) {
		t.Fatalf("forbiddenServices = %v, want %v (update go_fleet_runner and services-registry with it)", forbiddenServices, want)
	}
	for _, name := range want {
		if forbiddenKey(name) != name {
			t.Errorf("%s is not in its normalised form %q", name, forbiddenKey(name))
		}
	}
}

func TestForbiddenKeyMatchesEveryNameOfTheServiceAndNothingElse(t *testing.T) {
	for _, name := range []string{
		"fleet-graph", "go-fleet-graph", "go_fleet_graph", "GO_FLEET_GRAPH", "  apikey-service ", "go-apikey-service", "go_apikey_service",
		"fleet-secrets", "go-fleet-secrets", "go_fleet_secrets", "keystore-mesh", "go-keystore-mesh",
	} {
		if !identityForbidden(name) {
			t.Errorf("%q is a name of a forbidden service and was not matched", name)
		}
	}
	for _, name := range []string{
		"", "apikey-scanner", "go_apikey_scanner", "fleet-graph-ui", "fleet-memory", "go-fleet-memory", "fleet-secrets-backup", "keystore",
		"secrets", "graph", "domain-graph",
	} {
		if identityForbidden(name) {
			t.Errorf("%q is not a forbidden service but was matched", name)
		}
	}
}

func TestInitKeepsAForbiddenServiceSilent(t *testing.T) {
	for _, name := range []string{"fleet-graph", "go-fleet-graph", "go_apikey_service", "fleet-secrets", "go_fleet_secrets", "keystore-mesh"} {
		t.Run(name, func(t *testing.T) {
			resetState(t)
			hits := configureWriter(t)
			Init(name, "0.1.0")
			recordFive()
			if Enabled() || observing() {
				t.Fatalf("%s: Graph emission is on for a service that must never run the writer", name)
			}
			st := Stats()
			if st.EventsRecorded != 0 {
				t.Fatalf("%s: %d events were recorded, want 0", name, st.EventsRecorded)
			}
			if st.EventsDroppedInvalidIdentity < 5 {
				t.Fatalf("%s: dropped-for-identity counter = %d, want >= 5 (the drop must be visible)", name, st.EventsDroppedInvalidIdentity)
			}
			if ServiceID() != "" {
				t.Fatalf("%s: the package kept an identity (%q) that could be stamped on events", name, ServiceID())
			}
			if n := atomic.LoadInt64(hits); n != 0 {
				t.Fatalf("%s: the collector received %d requests, want 0", name, n)
			}
		})
	}
}

// The flush loop is the part that would send: prove it with a real tick for one identity.
func TestAForbiddenServiceNeverReachesTheCollectorAcrossAFlushTick(t *testing.T) {
	resetState(t)
	hits := configureWriter(t)
	Init("go-apikey-service", "0.1.0")
	recordFive()
	time.Sleep(1500 * time.Millisecond)
	if n := atomic.LoadInt64(hits); n != 0 {
		t.Fatalf("the collector received %d requests from a forbidden service across a flush tick", n)
	}
}

func TestAForbiddenIdentityInTheEnvironmentIsRefusedWhateverInitIsGiven(t *testing.T) {
	resetState(t)
	hits := configureWriter(t)
	t.Setenv("GRAPH_SERVICE_ID", "go-fleet-secrets")
	Init("fleet-memory", "0.1.0") // an ordinary default id, overridden by the operator's GRAPH_SERVICE_ID
	recordFive()
	if Enabled() || Stats().EventsRecorded != 0 || atomic.LoadInt64(hits) != 0 {
		t.Fatalf("GRAPH_SERVICE_ID naming a forbidden service was honoured: enabled=%v recorded=%d", Enabled(), Stats().EventsRecorded)
	}
}

func TestARecordBeforeInitWithAForbiddenEnvironmentIdentityIsDropped(t *testing.T) {
	resetState(t)
	configureWriter(t)
	t.Setenv("GRAPH_SERVICE_ID", "keystore-mesh")
	recordFive() // no Init yet: the bootstrap path decides
	if Enabled() || Stats().EventsRecorded != 0 {
		t.Fatalf("a record before Init emitted for a forbidden identity: enabled=%v recorded=%d", Enabled(), Stats().EventsRecorded)
	}
}

func TestTheForbiddenFlagIsStickyAcrossLaterInitCalls(t *testing.T) {
	resetState(t)
	configureWriter(t)
	Init("apikey-service", "0.1.0")
	Init("fleet-memory", "0.1.0") // nothing legitimate does this; it must not switch emission on
	recordFive()
	if Enabled() || Stats().EventsRecorded != 0 || ServiceID() != "" {
		t.Fatalf("a later Init un-forbade the process: enabled=%v recorded=%d id=%q", Enabled(), Stats().EventsRecorded, ServiceID())
	}
}

func TestOrdinaryServicesStillEmit(t *testing.T) {
	for _, name := range []string{"fleet-memory", "go_fleet_memory", "apikey-scanner", "fleet-graph-ui", "fleet-secrets-backup", "hiring-footprint"} {
		t.Run(name, func(t *testing.T) {
			resetState(t)
			configureWriter(t)
			Init(name, "0.1.0")
			recordFive()
			if !Enabled() || Stats().EventsRecorded != 5 {
				t.Fatalf("%s: an ordinary service did not emit: enabled=%v recorded=%d", name, Enabled(), Stats().EventsRecorded)
			}
		})
	}
}

func TestTheForbiddenWarningIsLoggedOnceAndOnlyWhenEmissionIsConfigured(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })

	// not configured to emit (no flag): silent, like the invalid-identity warning
	resetState(t)
	t.Setenv("GRAPH_ENABLED", "")
	Init("fleet-graph", "0.1.0")
	if strings.Contains(buf.String(), "must never run the Fleet Graph writer") {
		t.Fatalf("logged although Graph is not configured to emit:\n%s", buf.String())
	}

	// configured: exactly one line, however many times Init and Record run
	buf.Reset()
	resetState(t)
	configureWriter(t)
	Init("fleet-graph", "0.1.0")
	Init("fleet-graph", "0.1.0")
	recordFive()
	if n := strings.Count(buf.String(), "must never run the Fleet Graph writer"); n != 1 {
		t.Fatalf("the warning was logged %d times, want once:\n%s", n, buf.String())
	}
	for _, leak := range []string{"test-key", "GRAPH_API_KEY"} {
		if strings.Contains(buf.String(), leak) {
			t.Fatalf("the warning leaked %q", leak)
		}
	}
}
