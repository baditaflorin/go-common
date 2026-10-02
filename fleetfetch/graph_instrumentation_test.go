package fleetfetch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/graph"
)

const graphTestChildEnv = "FLEETFETCH_GRAPH_TRANSPORT_TEST_CHILD"

// Run the graph integration check in a child process so the graph package's
// process-wide configuration cannot leak into the other fleetfetch tests.
func TestDefaultCacheClientRecordsGraphEvent(t *testing.T) {
	if os.Getenv(graphTestChildEnv) == "1" {
		testDefaultCacheClientRecordsGraphEvent(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDefaultCacheClientRecordsGraphEvent$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, graphTestChildEnv+"=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, graphTestChildEnv+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated graph transport check failed: %v\n%s", err, output)
	}
}

func testDefaultCacheClientRecordsGraphEvent(t *testing.T) {
	t.Helper()
	events := make(chan graph.Batch, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			http.NotFound(w, r)
			return
		}
		var batch graph.Batch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("decode graph batch: %v", err)
			http.Error(w, "invalid batch", http.StatusBadRequest)
			return
		}
		select {
		case events <- batch:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()

	cache := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer cache.Close()

	t.Setenv("GRAPH_ENABLED", "true")
	t.Setenv("GRAPH_COLLECTOR_URL", collector.URL)
	t.Setenv("GRAPH_API_KEY", "graph-test-key")
	t.Setenv("GRAPH_FLUSH_INTERVAL", "1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	graph.Init("a11y-quick", "test")
	defer graph.Shutdown()

	client := NewClient(WithCacheURL(cache.URL))
	resp, err := client.cacheClient.Get(cache.URL)
	if err != nil {
		t.Fatalf("cache request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	select {
	case batch := <-events:
		if batch.Service != "a11y-quick" {
			t.Fatalf("graph batch service = %q, want a11y-quick", batch.Service)
		}
		if len(batch.Events) != 1 {
			t.Fatalf("graph batch events = %d, want 1", len(batch.Events))
		}
		event := batch.Events[0]
		if event.Direction != "out" || event.Caller != "a11y-quick" || event.Status != http.StatusOK {
			t.Fatalf("unexpected cache graph event: %+v", event)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Fleet Graph collector did not receive the internal cache call")
	}
}
