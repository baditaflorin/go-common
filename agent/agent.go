// Package agent defines the machine-readable "agent contract" that every
// fleet service can advertise so an AI agent (Claude, Codex, OpenCode, an
// MCP server, the catalog hub) can call it correctly without guessing the
// input shape, auth model, or output envelope.
//
// It is the agentic counterpart to server/capabilities.go (which lists
// query-flag knobs) and server/schema.go (which carries the envelope
// schema version). Where /capabilities says "these toggles exist" and
// /schema says "this is the envelope version", /agent.json says "here is
// exactly how to invoke me: one named tool, its input JSON Schema, the
// auth it needs, and what the response looks like."
//
// Why a dedicated package instead of bolting fields onto Capability:
// an agent tool is a higher-level unit than a query flag. A service
// exposes ONE tool (or a few) that an agent reasons about; capabilities
// are the lower-level dials *inside* that tool. Keeping them separate
// means the agent contract stays small and stable while capabilities can
// grow without changing the agent's mental model.
//
// Propagation model: the serving logic lives here, in go-common. A
// service opts in with a single option:
//
//	//go:embed agent.json
//	var agentFS embed.FS
//	srv := server.New(cfg, server.WithAgentFromEmbed(agentFS, "agent.json"))
//
// or, to reuse the fleet-wide default contract without shipping its own
// file:
//
//	srv := server.New(cfg, server.WithAgent(agent.DefaultTool(cfg.AppName, cfg.Version)))
//
// fleet-runner inject already refreshes go-common across the fleet via the
// replace ../go-common directive, so adding this package propagates the
// capability to every service on its next converge. Per-repo work is just
// the optional agent.json data file — no handler code.
package agent

import "encoding/json"

// Tool is the agent-facing contract for one callable unit of a service.
// It is intentionally close to the Model Context Protocol (MCP) "tool"
// object and to OpenAI/Anthropic function-calling "function" objects so a
// bridge can translate 1:1 without remapping.
type Tool struct {
	// Name is the stable tool identifier (typically the service slug).
	// Agents key on this; it must be unique within a server.
	Name string `json:"name"`
	// Summary is a one-line description for tool lists.
	Summary string `json:"summary"`
	// Description is the longer, agent-readable explanation.
	Description string `json:"description,omitempty"`
	// Category mirrors the fleet category (security, recon, web_analysis…).
	Category string `json:"category,omitempty"`
	// Tags are free-form fleet tags (go, kind-container…).
	Tags []string `json:"tags,omitempty"`
	// TRL is the technology-readiness level from the catalog, if known.
	TRL int `json:"trl,omitempty"`
	// InputSchema is a JSON Schema (draft-07 subset) describing the
	// parameters an agent must supply. The service's primary target
	// param (url/target/domain/…) should be marked required.
	InputSchema map[string]any `json:"input_schema"`
	// Auth describes what the gateway needs to authorize the call.
	Auth Auth `json:"auth"`
	// OutputShape is a short, human-readable note about the response
	// envelope (not a full schema — keep it cheap to render in a tool
	// list). The canonical envelope is response.Envelope.
	OutputShape string `json:"output_shape,omitempty"`
	// Examples are ready-to-run invocations (curl) for the catalog/hub.
	Examples []string `json:"examples,omitempty"`
	// Method is the HTTP method server.WithMCP uses to invoke this tool
	// against the service's own route. Empty defaults to GET.
	Method string `json:"method,omitempty"`
	// Path is the HTTP path server.WithMCP invokes, relative to the
	// service root. Empty defaults to "/". May contain "{name}"
	// placeholders (e.g. "/{type}/{name}") substituted from matching
	// InputSchema properties before the request is built — several
	// fleet services (go-fleet-dig's /{type}/{name}) take their
	// primary input from the path, not a query string. Whatever
	// InputSchema properties aren't consumed by a placeholder become
	// query parameters for GET/HEAD/DELETE, or a JSON body otherwise
	// (unless BodyField is set — see below).
	Path string `json:"path,omitempty"`
	// BodyField names the one InputSchema property (must be a string)
	// sent as the raw, un-encoded request body on non-GET methods,
	// instead of JSON-marshaling every property. Several fleet
	// services (go-fleet-md: POST the markdown itself, not a JSON
	// envelope) take their primary input as a raw body with the rest
	// of their knobs on the query string. Every other property still
	// becomes a query parameter, same as the GET/HEAD/DELETE case.
	// Ignored when Method is empty/GET/HEAD/DELETE.
	BodyField string `json:"body_field,omitempty"`
	// BodyContentType is the Content-Type sent with BodyField's raw
	// value. Defaults to "text/plain; charset=utf-8" when BodyField is
	// set and this is empty.
	BodyContentType string `json:"body_content_type,omitempty"`
}

// Auth describes the authorization an agent must attach.
type Auth struct {
	// Type is "api_key" for the fleet gateway, or "none".
	Type string `json:"type"`
	// Header is the request header the gateway expects (e.g. "X-API-Key").
	Header string `json:"header,omitempty"`
	// QueryParam is the query-param alternative (e.g. "api_key").
	QueryParam string `json:"query_param,omitempty"`
}

// Contract is the full payload served at GET /agent.json. It can carry one
// or more tools (most fleet services expose exactly one).
type Contract struct {
	// SchemaVersion lets drift checkers distinguish contract shapes.
	// Bump when Tool/Auth gain fields in a breaking way.
	SchemaVersion int    `json:"schema_version"`
	Service       string `json:"service"`
	Version       string `json:"version"`
	Tools         []Tool `json:"tools"`
}

// DefaultSchemaVersion is the contract schema version advertised when a
// service does not override it.
const DefaultSchemaVersion = 1

// DefaultAuth returns the standard fleet gateway auth descriptor.
func DefaultAuth() Auth {
	return Auth{Type: "api_key", Header: "X-API-Key", QueryParam: "api_key"}
}

// DefaultTool builds a minimal but valid agent Tool for a service using the
// fleet-wide conventions: a single required "target" string input (the
// primary fetch param), api_key auth, and the standard envelope note. This
// is what a service gets if it calls WithAgent(agent.DefaultTool(...))
// without shipping its own agent.json — enough for an agent to call it,
// with the service free to enrich later.
func DefaultTool(service, version string) Tool {
	return Tool{
		Name:        service,
		Summary:     service + " fleet service",
		Description: "Auto-described fleet service. Override with an embedded agent.json for a precise contract.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Primary input for the service (URL, domain, or query).",
				},
			},
			"required": []string{"target"},
		},
		Auth:        DefaultAuth(),
		OutputShape: "response.Envelope-wrapped JSON (see server/schema.go _schema_version).",
		Method:      "GET",
		Path:        "/",
	}
}

// DefaultContract builds a Contract with a single DefaultTool.
func DefaultContract(service, version string) Contract {
	return Contract{
		SchemaVersion: DefaultSchemaVersion,
		Service:       service,
		Version:       version,
		Tools:         []Tool{DefaultTool(service, version)},
	}
}

// JSON serialises the contract to indented JSON.
func (c Contract) JSON() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}

// FromJSON parses a Contract from JSON bytes (used by WithAgentFromEmbed).
func FromJSON(b []byte) (Contract, error) {
	var c Contract
	err := json.Unmarshal(b, &c)
	return c, err
}
