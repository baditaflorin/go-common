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
