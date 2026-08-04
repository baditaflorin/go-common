package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/baditaflorin/go-common/agent"
	"github.com/baditaflorin/go-common/config"
)

// TestWithMCPRequiresAgentContract asserts the fail-fast: WithMCP with no
// WithAgent/WithAgentFromEmbed has nothing to expose and must panic rather
// than silently serving an empty tool list.
func TestWithMCPRequiresAgentContract(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic: WithMCP with no agent contract")
		}
	}()
	cfg := config.Load("test-mcp-no-contract", "1.0.0")
	New(cfg, WithMCP())
}

// TestWithMCPEndToEnd performs a real MCP handshake over the wire (not
// just an in-process handler call): initialize, tools/list, tools/call —
// using the official SDK's own client, against a server built with
// server.New(WithAgent(...), WithMCP()) and a real underlying handler
// mounted the same way server.Run mounts it.
func TestWithMCPEndToEnd(t *testing.T) {
	echoHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"echo":"` + r.URL.Query().Get("target") + `"}`)) //nolint:errcheck
	}

	cfg := config.Load("test-mcp-e2e", "1.2.3")
	contract := agent.DefaultContract(cfg.AppName, cfg.Version)
	srv := New(cfg, WithAgent(contract), WithMCP())
	srv.Mux.HandleFunc("/", echoHandler)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	transport := &sdkmcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != cfg.AppName {
		t.Fatalf("tools/list = %+v, want one tool named %q", tools.Tools, cfg.AppName)
	}

	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      cfg.AppName,
		Arguments: map[string]any{"target": "example.com"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool returned IsError=true: %+v", result.Content)
	}
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("CallTool content[0] type = %T, want *TextContent", result.Content[0])
	}
	if want := `{"echo":"example.com"}`; text.Text != want {
		t.Fatalf("CallTool text = %q, want %q", text.Text, want)
	}
}
