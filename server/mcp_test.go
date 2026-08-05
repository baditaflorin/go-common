package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/baditaflorin/go-common/agent"
	"github.com/baditaflorin/go-common/config"
)

// TestSubstitutePathParams covers the go-fleet-dig shape (path-embedded
// params, e.g. /{type}/{name}) that a pure query-string convention can't
// express.
func TestSubstitutePathParams(t *testing.T) {
	path, remaining := substitutePathParams("/{type}/{name}", map[string]any{
		"type": "A", "name": "example.com", "json": true,
	})
	if want := "/A/example.com"; path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if len(remaining) != 1 || remaining["json"] != true {
		t.Fatalf("remaining = %+v, want only {json: true}", remaining)
	}
}

func TestSubstitutePathParamsMissingArgSubstitutesEmpty(t *testing.T) {
	path, remaining := substitutePathParams("/{type}/{name}", map[string]any{"type": "A"})
	if want := "/A/"; path != want {
		t.Fatalf("path = %q, want %q (missing arg -> empty, not an error)", path, want)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %+v, want empty", remaining)
	}
}

// TestBuildToolRequestBodyField covers the go-fleet-md shape: a raw
// (non-JSON) request body plus separate query-string knobs.
func TestBuildToolRequestBodyField(t *testing.T) {
	req, err := buildToolRequest(context.Background(), http.MethodPost, "/", "markdown", "",
		map[string]any{"markdown": "# hi", "to": "html"})
	if err != nil {
		t.Fatalf("buildToolRequest: %v", err)
	}
	if req.URL.RequestURI() != "/?to=html" {
		t.Fatalf("URI = %q, want /?to=html (markdown must not leak into the query string)", req.URL.RequestURI())
	}
	body, _ := io.ReadAll(req.Body)
	if string(body) != "# hi" {
		t.Fatalf("body = %q, want raw %q, not JSON-wrapped", body, "# hi")
	}
	if ct := req.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the text/plain default", ct)
	}
}

// TestWithMCPEndToEndPathParams proves the full wire path for a
// go-fleet-dig-shaped tool: initialize -> tools/list -> tools/call with
// path-embedded args, replayed against a real path-pattern handler.
func TestWithMCPEndToEndPathParams(t *testing.T) {
	digHandler := func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.Trim(r.URL.Path, "/"), "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(parts[0] + " " + parts[1])) //nolint:errcheck
	}

	cfg := config.Load("test-mcp-path-params", "1.0.0")
	contract := agent.Contract{
		Service: cfg.AppName,
		Version: cfg.Version,
		Tools: []agent.Tool{{
			Name: "dig",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{"type": "string"},
					"name": map[string]any{"type": "string"},
				},
				"required": []string{"type", "name"},
			},
			Method: "GET",
			Path:   "/{type}/{name}",
		}},
	}
	srv := New(cfg, WithAgent(contract), WithMCP())
	srv.Mux.HandleFunc("/", digHandler)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      "dig",
		Arguments: map[string]any{"type": "A", "name": "example.com"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool IsError=true: %+v", result.Content)
	}
	text := result.Content[0].(*sdkmcp.TextContent).Text
	if want := "A example.com"; text != want {
		t.Fatalf("CallTool text = %q, want %q", text, want)
	}
}

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

// headerInjectingRoundTripper adds a fixed header to every outbound request
// — stands in for what nginx does with X-Forwarded-For before a real
// request reaches the service.
type headerInjectingRoundTripper struct {
	header, value string
	base          http.RoundTripper
}

func (rt *headerInjectingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(rt.header, rt.value)
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// TestWithMCPForwardsClientHeaders locks in the fix for a bug reproduced
// live 2026-08-05: mcpToolHandler synthesized a brand-new *http.Request for
// every tools/call with no headers and no RemoteAddr carried over from the
// real inbound request, so any handler that inspects the caller (an IP-echo
// service reading X-Forwarded-For, go-fleet-ip's ?json response) saw an
// anonymous request and returned empty fields — even though the exact same
// request over plain HTTP worked correctly. copyRequestContext must
// propagate caller headers like X-Forwarded-For onto the synthesized
// request so the /mcp surface never disagrees with the HTTP surface it
// bridges to (see this file's package doc).
func TestWithMCPForwardsClientHeaders(t *testing.T) {
	var gotXFF string
	echoXFF := func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"forwarded_for":"` + gotXFF + `"}`)) //nolint:errcheck
	}

	cfg := config.Load("test-mcp-xff", "1.0.0")
	contract := agent.DefaultContract(cfg.AppName, cfg.Version)
	srv := New(cfg, WithAgent(contract), WithMCP())
	srv.Mux.HandleFunc("/", echoXFF)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	transport := &sdkmcp.StreamableClientTransport{
		Endpoint: ts.URL + "/mcp",
		HTTPClient: &http.Client{
			Transport: &headerInjectingRoundTripper{header: "X-Forwarded-For", value: "203.0.113.42"},
		},
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: cfg.AppName, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool returned IsError=true: %+v", result.Content)
	}
	if gotXFF != "203.0.113.42" {
		t.Fatalf("handler saw X-Forwarded-For = %q, want %q (header lost between /mcp and the replayed request)", gotXFF, "203.0.113.42")
	}
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("CallTool content[0] type = %T, want *TextContent", result.Content[0])
	}
	if want := `{"forwarded_for":"203.0.113.42"}`; text.Text != want {
		t.Fatalf("CallTool text = %q, want %q", text.Text, want)
	}
}
