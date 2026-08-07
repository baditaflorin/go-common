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
	"regexp"
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

		httpReq, err := buildToolRequest(ctx, method, path, t.BodyField, t.BodyContentType, args)
		if err != nil {
			return mcpErrorResult(err.Error()), nil
		}
		copyRequestContext(req, httpReq)

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

// copyRequestContextAllowlist is every header this bridge is willing to
// carry from the original inbound MCP call onto the synthesized in-process
// replay — caller-identity and content-negotiation facts a handler
// legitimately inspects (an IP-echo service, User-Agent-derived behaviour,
// locale negotiation, the fleet's own X-Fleet-Caller convention). Confirmed
// against real handler usage across the fleet (go-fleet-ip,
// go_infrastructure_fetch_cache, go-fleet-mcp-gateway) 2026-08-07.
//
// This is deliberately an ALLOWLIST, not a denylist of known-dangerous
// headers. A denylist only stays safe as long as every future privileged
// header gets added to it by hand; an allowlist is safe by construction —
// anything not named here never reaches the replayed request, including
// X-Admin-Token or any other credential a caller might carry for reasons
// unrelated to this call (a shared client, a misconfigured proxy, a
// forwarding bug upstream). Flagged as a real gap 2026-08-07: the previous
// denylist (Authorization/X-API-Key/Content-Type/Content-Length only)
// would have silently forwarded X-Admin-Token straight through to any
// handler that happened to check for it.
var copyRequestContextAllowlist = map[string]bool{
	"Accept":            true,
	"Accept-Language":   true,
	"Cache-Control":     true,
	"User-Agent":        true,
	"X-Fleet-Caller":    true,
	"X-Forwarded-For":   true,
	"X-Forwarded-Host":  true,
	"X-Forwarded-Proto": true,
	"X-Real-Ip":         true,
}

// copyRequestContext propagates the caller's real request-identity headers
// (see copyRequestContextAllowlist) from the original inbound HTTP request
// onto the synthesized in-process replay, so handlers that inspect the
// caller see the same facts through /mcp as they would through the plain
// HTTP surface. Without this, every tools/call arrives at the handler as an
// anonymous request with no RemoteAddr and no headers — reproduced live
// 2026-08-05 against go-fleet-ip, whose ?json response came back with every
// field empty.
//
// Content-Type / Content-Length are never carried — buildToolRequest already
// set them correctly for the synthesized body, and copying the *original*
// request's values would describe the wrong body. Authorization / X-API-Key
// are never carried either: per this file's package doc, auth already ran
// against the inbound request before this replay happens, so the synthetic
// request does not need to carry credentials of its own, and not copying
// them keeps this bridge from becoming a second place a credential is
// handled.
func copyRequestContext(req *sdkmcp.CallToolRequest, upstream *http.Request) {
	if req.Extra == nil || req.Extra.Header == nil {
		return
	}
	for k, vv := range req.Extra.Header {
		if !copyRequestContextAllowlist[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			upstream.Header.Add(k, v)
		}
	}
}

// pathParamPattern matches "{name}" placeholders in a Tool.Path.
var pathParamPattern = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// substitutePathParams replaces every "{name}" placeholder in path with
// args["name"] (URL-path-escaped) and returns the substituted path plus
// a copy of args with the consumed keys removed, so callers can safely
// turn whatever's left into query parameters or a body without
// double-sending the path-bound values.
//
// A placeholder with no matching arg substitutes as empty — the
// resulting 404/400 from the real handler is a clearer signal than a
// bridge-level error for a case that's really "the agent didn't supply
// a required field."
func substitutePathParams(path string, args map[string]any) (string, map[string]any) {
	remaining := make(map[string]any, len(args))
	for k, v := range args {
		remaining[k] = v
	}
	substituted := pathParamPattern.ReplaceAllStringFunc(path, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := remaining[name]
		if !ok {
			return ""
		}
		delete(remaining, name)
		return url.PathEscape(scalarString(v))
	})
	return substituted, remaining
}

// buildToolRequest builds the *http.Request for one tools/call. Path
// placeholders are substituted first (see substitutePathParams); of
// what's left:
//   - GET/HEAD/DELETE: query parameters.
//   - non-GET with bodyField set: args[bodyField] (a string) becomes
//     the raw request body verbatim, everything else becomes query
//     parameters — the go-fleet-md shape (raw body + query knobs).
//   - non-GET otherwise: the whole remaining map, JSON-marshaled, is
//     the body — the default shape for services with a JSON-object API.
func buildToolRequest(ctx context.Context, method, path, bodyField, bodyContentType string, args map[string]any) (*http.Request, error) {
	path, args = substitutePathParams(path, args)

	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodDelete:
		return http.NewRequestWithContext(ctx, method, withQuery(path, args), nil)
	default:
		if bodyField != "" {
			raw, _ := args[bodyField].(string)
			delete(args, bodyField)
			r, err := http.NewRequestWithContext(ctx, method, withQuery(path, args), strings.NewReader(raw))
			if err != nil {
				return nil, err
			}
			ct := bodyContentType
			if ct == "" {
				ct = "text/plain; charset=utf-8"
			}
			r.Header.Set("Content-Type", ct)
			return r, nil
		}
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

// withQuery appends args as a query string onto path (path may already
// have been through substitutePathParams).
func withQuery(path string, args map[string]any) string {
	q := url.Values{}
	for k, v := range args {
		q.Set(k, scalarString(v))
	}
	if enc := q.Encode(); enc != "" {
		return path + "?" + enc
	}
	return path
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
