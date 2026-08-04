package agent

import (
	"embed"
	"fmt"
	"io/fs"
)

// LoadEmbed reads an agent.json from an embedded filesystem (the typical
// service pattern: //go:embed agent.json var agentFS embed.FS) and parses
// it into a Contract. Service identity (service/version) is taken from the
// caller so a service can ship a contract that omits those fields.
//
// If the embedded file is missing or invalid, an error is returned — the
// server option should fail fast rather than serve a silent default that
// masks a broken embed.
func LoadEmbed(fsys embed.FS, name, service, version string) (Contract, error) {
	raw, err := fsys.Open(name)
	if err != nil {
		return Contract{}, fmt.Errorf("agent: open embedded %q: %w", name, err)
	}
	defer raw.Close()

	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Contract{}, fmt.Errorf("agent: read embedded %q: %w", name, err)
	}
	c, err := FromJSON(data)
	if err != nil {
		return Contract{}, fmt.Errorf("agent: parse embedded %q: %w", name, err)
	}
	if c.Service == "" {
		c.Service = service
	}
	if c.Version == "" {
		c.Version = version
	}
	if c.SchemaVersion == 0 {
		c.SchemaVersion = DefaultSchemaVersion
	}
	return c, nil
}

// ExampleAgentJSON is a reference agent.json a service can copy and edit.
// Ship it as agent.json next to main.go with:
//
//	//go:embed agent.json
//	var agentFS embed.FS
//
// and register with server.WithAgentFromEmbed(agentFS, "agent.json").
const ExampleAgentJSON = `{
  "schema_version": 1,
  "tools": [
    {
      "name": "analyze-headers",
      "summary": "OWASP secure-header auditor",
      "description": "Fetches a target URL and grades every security-relevant response header (HSTS, CSP, COOP/COEP/CORP, Permissions-Policy, X-Frame-Options, etc.) with per-header A-F grades, a composite score, and prioritised remediation steps.",
      "category": "security",
      "tags": ["go", "security", "headers"],
      "trl": 6,
      "input_schema": {
        "type": "object",
        "properties": {
          "target": {
            "type": "string",
            "description": "Target URL to audit (e.g. https://example.com)."
          }
        },
        "required": ["target"]
      },
      "auth": {
        "type": "api_key",
        "header": "X-API-Key",
        "query_param": "api_key"
      },
      "output_shape": "response.Envelope-wrapped audit: per-header grades, composite_grade, score, recommendations.",
      "examples": [
        "curl 'https://analyze-headers.0crawl.com/?url=https://example.com&api_key=<KEY>'"
      ]
    }
  ]
}
`
