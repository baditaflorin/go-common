package graph

import (
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/env"
)

func declared(t *testing.T) map[string]env.Info {
	t.Helper()
	out := map[string]env.Info{}
	for _, i := range env.Default.Snapshot(nil) {
		out[i.Name] = i
	}
	return out
}

func TestEveryVariableGraphReadsIsDeclared(t *testing.T) {
	got := declared(t)
	for _, name := range []string{
		"GRAPH_ENABLED", "GRAPH_COLLECTOR_URL", "GRAPH_API_KEY", "GRAPH_API_KEY_FILE", "GRAPH_READER_API_KEY",
		"GRAPH_SERVICE_ID", "GRAPH_TARGET_ALIASES", "GRAPH_TRUSTED_CALLER_IPS", "GRAPH_SAMPLE_RATE",
		"GRAPH_BUFFER_SIZE", "GRAPH_FLUSH_INTERVAL", "GRAPH_FLUSH_BATCH", "FLEET_NODE",
	} {
		i, ok := got[name]
		if !ok {
			t.Errorf("%s is not declared", name)
			continue
		}
		if i.Owner != "graph" || i.Doc == "" {
			t.Errorf("%s: owner=%q doc=%q, want owner graph and a description", name, i.Owner, i.Doc)
		}
	}
	for _, name := range []string{"GRAPH_API_KEY", "GRAPH_READER_API_KEY"} {
		if !got[name].Secret {
			t.Errorf("%s holds a credential and must be declared Secret", name)
		}
	}
}

func TestADeclaredCredentialNeverAppearsInTheListing(t *testing.T) {
	t.Setenv("GRAPH_API_KEY", "writer-key-value-123")
	t.Setenv("GRAPH_READER_API_KEY", "reader-key-value-456")
	for _, i := range env.Default.Snapshot([]string{"GRAPH_API_KEY=writer-key-value-123", "GRAPH_READER_API_KEY=reader-key-value-456"}) {
		if strings.Contains(i.Value+i.Default, "key-value") {
			t.Fatalf("%s leaked a credential: %+v", i.Name, i)
		}
	}
}

func TestStartupValidationCatchesTheMistakesGraphIgnores(t *testing.T) {
	probs := env.Default.Validate([]string{
		"GRAPH_ENABLED=ture", // read as false: graph would silently stay off
		"GRAPH_COLLECTER_URL=https://fleet-graph.0exec.com", // a typo: the real variable stays unset
		"GRAPH_COLLECTOR_URL=http://evil.example",           // not the canonical host
		"GRAPH_SERVICE_ID=Not_A_Label",
		"FLEET_NODE=has spaces",
		"GRAPH_TRUSTED_CALLER_IPS=10.0.0.1,not-an-ip",
		"GRAPH_USE_EGRESS_PROXY=true", // removed
	})
	want := map[string]string{
		"GRAPH_ENABLED":            "boolean",
		"GRAPH_COLLECTER_URL":      "did you mean GRAPH_COLLECTOR_URL",
		"GRAPH_COLLECTOR_URL":      "",
		"GRAPH_SERVICE_ID":         "service id",
		"FLEET_NODE":               "DNS label",
		"GRAPH_TRUSTED_CALLER_IPS": "not-an-ip",
		"GRAPH_USE_EGRESS_PROXY":   "v0.102.60",
	}
	for name, substr := range want {
		var found *env.Problem
		for i := range probs {
			if probs[i].Name == name {
				found = &probs[i]
			}
		}
		if found == nil {
			t.Errorf("no problem reported for %s", name)
		} else if !strings.Contains(found.Message, substr) {
			t.Errorf("%s: %q does not mention %q", name, found.Message, substr)
		}
	}
}

func TestAGoodGraphEnvironmentHasNoProblems(t *testing.T) {
	probs := env.Default.Validate([]string{
		"GRAPH_ENABLED=true", "GRAPH_COLLECTOR_URL=https://fleet-graph.0exec.com", "GRAPH_SERVICE_ID=fleet-demo",
		"FLEET_NODE=dockerhost", "GRAPH_SAMPLE_RATE=0.5", "GRAPH_BUFFER_SIZE=2048", "GRAPH_FLUSH_INTERVAL=5",
		"GRAPH_FLUSH_BATCH=100", "GRAPH_TRUSTED_CALLER_IPS=10.0.0.1, 10.0.0.2",
	})
	for _, p := range probs {
		if strings.HasPrefix(p.Name, "GRAPH_") || p.Name == "FLEET_NODE" {
			t.Errorf("a valid value was reported: %+v", p)
		}
	}
}
