// mcp.go bridges a service's agent contract (see agent/agent.go and
// WithAgent/WithAgentFromEmbed) onto a real Model Context Protocol
// Streamable HTTP endpoint at GET/POST /mcp, using the official
// github.com/modelcontextprotocol/go-sdk.
//
// Why the bridge dispatches through s.Mux instead of re-implementing each
// tool: a service already has exactly one correct implementation of its
// behaviour — its HTTP handler. Building a second implementation for MCP
// would drift from the first the moment either one changes. Instead, a
// tools/call synthesises an *http.Request from the tool's declared
// Method/Path/InputSchema and replays it against s.Mux directly
// (httptest.NewRecorder, no network hop), so the MCP surface can never
// disagree with the HTTP surface.
//
// Auth is not re-implemented either. /mcp is just another route on s.Mux,
// so it sits behind the same middleware chain (WithKeystoreAuth et al.)
// as every other route — the same Authorization: Bearer <key> the fleet
// keystore already issues. Because the in-process replay happens after
// that chain has already authorized the inbound request, the synthetic
// request does not need to carry credentials of its own.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/baditaflorin/go-common/agent"
)

// WithMCP mounts a Model Context Protocol Streamable HTTP endpoint at
// /mcp. It requires an agent contract — WithAgent or WithAgentFromEmbed,
// in either order relative to this option, since both are resolved into
// s.AgentContract before /mcp is mounted (see New()). A service with no
// contract has nothing to expose as MCP tools; New panics in that case
// rather than silently serving an empty tool list.
func WithMCP() Option {
	return func(s *Server) { s.mcpEnabled = true }
}

// mountMCP builds one MCP tool per agent.Tool in s.AgentContract and
// mounts them at /mcp. Called from New() after every Option has run.
func mountMCP(s *Server) {
	if len(s.AgentContract.Tools) == 0 {
		panic("server: WithMCP requires an agent contract with at least one tool (see WithAgent / WithAgentFromEmbed)")
	}

	mcpSrv := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    s.Config.AppName,
		Version: s.Config.Version,
	}, nil)

	for _, t := range s.AgentContract.Tools {
		mcpSrv.AddTool(toMCPTool(t), mcpToolHandler(s, t))
	}

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return mcpSrv
	}, nil)
	s.Mux.Handle("/mcp", handler)
}

// toMCPTool translates one agent.Tool into the SDK's wire Tool shape.
// InputSchema is passed through as the raw map[string]any it already is
// — AddTool accepts any JSON-Schema-marshalable value and only requires
// "type":"object" at the top level, which every agent.Tool schema
// (DefaultTool included) already satisfies.
func toMCPTool(t agent.Tool) *sdkmcp.Tool {
	desc := t.Description
	if desc == "" {
		desc = t.Summary
	}
	schema := t.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	return &sdkmcp.Tool{
		Name:        t.Name,
		Description: desc,
		InputSchema: schema,
	}
}

// mcpToolHandler returns an MCP ToolHandler that replays a tools/call as
// an in-process HTTP request against s.Mux, using t.Method/t.Path
// (defaulting to GET "/") to build the request and t's arguments as
// query parameters (GET/HEAD/DELETE) or a JSON body (everything else).
func mcpToolHandler(s *Server, t agent.Tool) sdkmcp.ToolHandler {
	method := t.Method
	if method == "" {
		method = http.MethodGet
	}
	path := t.Path
	if path == "" {
		path = "/"
	}

	return func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return mcpErrorResult("invalid arguments: " + err.Error()), nil
			}
		}

		httpReq, err := buildToolRequest(ctx, method, path, args)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}

		rec := httptest.NewRecorder()
		s.Mux.ServeHTTP(rec, httpReq)

		body, _ := io.ReadAll(rec.Result().Body)
		result := &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(body)}},
		}
		if rec.Code >= 400 {
			result.IsError = true
		}
		return result, nil
	}
}

// buildToolRequest encodes args as query parameters for GET/HEAD/DELETE
// (the fleet convention most single-purpose services follow — a primary
// ?target= or similar param), or as a JSON body for every other method.
func buildToolRequest(ctx context.Context, method, path string, args map[string]any) (*http.Request, error) {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodDelete:
		q := url.Values{}
		for k, v := range args {
			q.Set(k, scalarString(v))
		}
		target := path
		if enc := q.Encode(); enc != "" {
			target += "?" + enc
		}
		return http.NewRequestWithContext(ctx, method, target, nil)
	default:
		body, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("encode arguments: %w", err)
		}
		r, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		return r, nil
	}
}

// scalarString renders one decoded JSON argument value as a query-string
// value. json.Unmarshal into map[string]any produces float64/string/
// bool/nil/[]any/map[string]any; anything non-scalar is JSON-re-encoded
// rather than dropped, so a caller passing a structured value still gets
// something on the wire instead of silent data loss.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func mcpErrorResult(msg string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: msg}},
		IsError: true,
	}
}
