package proxysupplier

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/graph"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("GRAPH_ENABLED", "true")
	_ = os.Setenv("GRAPH_COLLECTOR_URL", "http://127.0.0.1:43189")
	_ = os.Setenv("GRAPH_API_KEY", "test-only-writer-key")
	_ = os.Setenv("GRAPH_API_KEY_FILE", "")
	_ = os.Setenv("GRAPH_SERVICE_ID", "proxysupplier-test")
	_ = os.Setenv("GRAPH_SAMPLE_RATE", "1")
	_ = os.Setenv("GRAPH_FLUSH_INTERVAL", "3600")
	graph.Init("proxysupplier-test", "test")
	os.Exit(m.Run())
}

type graphTestSupplier struct{}

func (graphTestSupplier) Name() string     { return "graph-test" }
func (graphTestSupplier) ProxyURL() string { return "http://proxy.invalid:8080" }
func (graphTestSupplier) Bypass(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func TestHTTPClientRecordsOutboundGraphEvent(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	client := HTTPClient(graphTestSupplier{}, time.Second)
	if client == nil {
		t.Fatal("HTTPClient() returned nil for a configured supplier")
	}
	before := graph.Stats().EventsRecorded
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("client.Get(%q): %v", target.URL, err)
	}
	_ = resp.Body.Close()
	if got := graph.Stats().EventsRecorded; got != before+1 {
		t.Fatalf("outbound graph events = %d, want %d", got, before+1)
	}
}
