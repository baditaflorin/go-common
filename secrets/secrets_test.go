package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewFromEnvRequiresApprovedHTTPSHost(t *testing.T) {
	for _, raw := range []string{
		"http://fleet-secrets.0exec.com",
		"https://attacker.example",
		"https://user@fleet-secrets.0exec.com",
		"https://fleet-secrets.0exec.com/path",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := NewFromEnv(func(key string) string {
				if key == "FLEET_SECRETS_URL" {
					return raw
				}
				if key == "FLEET_SECRETS_API_KEY" {
					return "test-key"
				}
				return ""
			}, nil)
			if err == nil {
				t.Fatal("unsafe URL accepted")
			}
		})
	}
}

func TestNewFromEnvReadsProtectedKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewFromEnv(func(key string) string {
		if key == "FLEET_SECRETS_API_KEY_FILE" {
			return path
		}
		return ""
	}, nil)
	if err != nil || client.apiKey != "test-key" {
		t.Fatalf("NewFromEnv protected file: client=%v err=%v", client != nil, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromEnv(func(key string) string {
		if key == "FLEET_SECRETS_API_KEY_FILE" {
			return path
		}
		return ""
	}, nil); err == nil || errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("unsafe file was accepted or reported as missing: %v", err)
	}
}

func TestValidSecretName(t *testing.T) {
	for _, name := range []string{"pyroscope-user", "openobserve_otlp_ingestion_token", "a.b"} {
		if !validSecretName(name) {
			t.Errorf("valid name rejected: %q", name)
		}
	}
	for _, name := range []string{"", "../secret", "name?query", "contains space"} {
		if validSecretName(name) {
			t.Errorf("invalid name accepted: %q", name)
		}
	}
}
