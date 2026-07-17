package agent

import (
	"embed"
	"testing"
)

//go:embed testdata/example.json
var testFS embed.FS

func TestDefaultToolShape(t *testing.T) {
	tool := DefaultTool("demo", "1.2.3")
	if tool.Name != "demo" {
		t.Fatalf("Name = %q, want demo", tool.Name)
	}
	if tool.InputSchema["type"] != "object" {
		t.Fatalf("input schema type = %v, want object", tool.InputSchema["type"])
	}
	props, ok := tool.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("input schema properties missing")
	}
	if _, ok := props["target"]; !ok {
		t.Fatalf("input schema missing required 'target' property")
	}
	if tool.Auth.Type != "api_key" || tool.Auth.Header != "X-API-Key" {
		t.Fatalf("auth = %+v, want api_key/X-API-Key", tool.Auth)
	}
}

func TestLoadEmbedFillsIdentity(t *testing.T) {
	c, err := LoadEmbed(testFS, "testdata/example.json", "svc", "9.9.9")
	if err != nil {
		t.Fatalf("LoadEmbed: %v", err)
	}
	if c.Service != "svc" {
		t.Fatalf("Service = %q, want svc (filled from caller)", c.Service)
	}
	if c.Version != "9.9.9" {
		t.Fatalf("Version = %q, want 9.9.9", c.Version)
	}
	if c.SchemaVersion != DefaultSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", c.SchemaVersion, DefaultSchemaVersion)
	}
	if len(c.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(c.Tools))
	}
}

func TestContractJSONRoundTrips(t *testing.T) {
	c := DefaultContract("demo", "1.0.0")
	b, err := c.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	got, err := FromJSON(b)
	if err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if got.Service != "demo" || len(got.Tools) != 1 {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}
