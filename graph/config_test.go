package graph

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGraphWriterAPIKeyFilePrecedence(t *testing.T) {
	t.Setenv("GRAPH_API_KEY", "legacy-env-key")
	keyPath := filepath.Join(t.TempDir(), "graph-writer-key")
	if err := os.WriteFile(keyPath, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRAPH_API_KEY_FILE", keyPath)

	if got := loadGraphWriterAPIKey(); got != "file-key" {
		t.Fatalf("loadGraphWriterAPIKey() = %q, want file-backed credential", got)
	}
}

func TestLoadGraphWriterAPIKeyFileFailureDoesNotFallBack(t *testing.T) {
	t.Setenv("GRAPH_API_KEY", "legacy-env-key")
	for name, contents := range map[string][]byte{
		"missing": nil,
		"empty":   {},
	} {
		t.Run(name, func(t *testing.T) {
			keyPath := filepath.Join(t.TempDir(), "graph-writer-key")
			if contents != nil {
				if err := os.WriteFile(keyPath, contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("GRAPH_API_KEY_FILE", keyPath)
			if got := loadGraphWriterAPIKey(); got != "" {
				t.Fatalf("loadGraphWriterAPIKey() = %q, want empty with unavailable file", got)
			}
		})
	}
}

func TestLoadGraphWriterAPIKeyEnvironmentCompatibility(t *testing.T) {
	t.Setenv("GRAPH_API_KEY", " legacy-env-key ")
	t.Setenv("GRAPH_API_KEY_FILE", " ")
	if got := loadGraphWriterAPIKey(); got != "legacy-env-key" {
		t.Fatalf("loadGraphWriterAPIKey() = %q, want trimmed compatibility credential", got)
	}
}

func TestFileBackedWriterKeyReloadsAndFailsClosed(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "graph-writer-key")
	t.Setenv("GRAPH_ENABLED", "true")
	t.Setenv("GRAPH_COLLECTOR_URL", "https://fleet-graph.0exec.com")
	t.Setenv("GRAPH_API_KEY", "legacy-env-key")
	t.Setenv("GRAPH_API_KEY_FILE", keyPath)
	cfg := loadConfig()
	if cfg.eventEmissionEnabled() {
		t.Fatal("missing configured file must fail closed")
	}
	if err := os.WriteFile(keyPath, []byte("first-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cfg.currentWriterAPIKey(); got != "first-key" {
		t.Fatalf("first current key = %q, want file value", got)
	}
	if err := os.WriteFile(keyPath, []byte("second-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cfg.currentWriterAPIKey(); got != "second-key" {
		t.Fatalf("rotated current key = %q, want replacement file value", got)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if got := cfg.currentWriterAPIKey(); got != "" {
		t.Fatalf("missing file key = %q, want fail-closed empty value", got)
	}
}
