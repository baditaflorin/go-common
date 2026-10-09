package profiling

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pyroscope "github.com/grafana/pyroscope-go"
)

func TestMakeConfigRequiresStableIdentityAndSecureRemoteEndpoint(t *testing.T) {
	tests := []struct {
		name, service, address, user, password string
		wantErr                                string
	}{
		{name: "missing service", address: "https://profiles.example.com", user: "user", password: "pass", wantErr: "service name"},
		{name: "remote HTTP", service: "catalog-api", address: "http://profiles.example.com", user: "user", password: "pass", wantErr: "HTTPS"},
		{name: "remote without auth", service: "catalog-api", address: "https://profiles.example.com", wantErr: "credentials"},
		{name: "URL credentials rejected", service: "catalog-api", address: "https://user:pass@profiles.example.com", user: "user", password: "pass", wantErr: "without credentials"},
		{name: "paired auth required", service: "catalog-api", address: "http://127.0.0.1:4040", user: "user", wantErr: "both be configured"},
		{name: "local HTTP allowed", service: "catalog-api", address: "http://127.0.0.1:4040"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := makeConfig(tt.service, tt.address, "prod", "1.2.3", tt.user, tt.password, "")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("makeConfig: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestMakeConfigBoundsUploadAndLimitsProfileSet(t *testing.T) {
	cfg, err := makeConfig("catalog-api", "https://profiles.example.com", "prod", "1.2.3", "user", "pass", "20s")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ApplicationName != "catalog-api" || cfg.UploadRate != 20*time.Second {
		t.Fatalf("unexpected config identity or upload rate: name=%q rate=%s", cfg.ApplicationName, cfg.UploadRate)
	}
	if !cfg.DisableGCRuns {
		t.Fatal("DisableGCRuns should avoid profiler-triggered full collections")
	}
	if len(cfg.ProfileTypes) != 4 {
		t.Fatalf("profile types = %d, want CPU, alloc-space, in-use-space, goroutines", len(cfg.ProfileTypes))
	}
	if cfg.Tags["environment"] != "prod" || cfg.Tags["version"] != "1.2.3" {
		t.Fatalf("stable deployment tags missing: %#v", cfg.Tags)
	}
	for _, value := range []string{"1s", "61s", "not-a-duration"} {
		if _, err := makeConfig("catalog-api", "https://profiles.example.com", "", "", "user", "pass", value); err == nil {
			t.Errorf("upload rate %q accepted; want rejection", value)
		}
	}
}

func TestReadSecretFileIsBoundedAndDoesNotExposeContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("  private-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(userFileEnv, path)
	got, err := readSecretFile(userFileEnv)
	if err != nil || got != "private-value" {
		t.Fatalf("readSecretFile = %q, %v", got, err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxSecretBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = readSecretFile(userFileEnv)
	if err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("oversized secret error leaked data or was not rejected: %v", err)
	}
}

func TestStartFromEnvWithoutEndpointIsNoop(t *testing.T) {
	t.Setenv(serverAddressEnv, "")
	stop, err := StartFromEnv("catalog-api")
	if err != nil {
		t.Fatalf("StartFromEnv without endpoint: %v", err)
	}
	stop()
}

func TestStartFromEnvSharesOneProfilerPerService(t *testing.T) {
	processProfiler.Lock()
	previousService, previousStop := processProfiler.serviceName, processProfiler.stop
	processProfiler.serviceName, processProfiler.stop = "", nil
	processProfiler.Unlock()
	t.Cleanup(func() {
		processProfiler.Lock()
		processProfiler.serviceName, processProfiler.stop = previousService, previousStop
		processProfiler.Unlock()
	})

	t.Setenv(serverAddressEnv, "http://127.0.0.1:4040")
	t.Setenv(userFileEnv, "")
	t.Setenv(passwordFileEnv, "")
	t.Setenv(userSecretEnv, "")
	t.Setenv(passwordSecretEnv, "")

	startCalls, stopCalls := 0, 0
	previousStart := startProfiler
	startProfiler = func(pyroscope.Config) (StopFunc, error) {
		startCalls++
		return func() { stopCalls++ }, nil
	}
	t.Cleanup(func() { startProfiler = previousStart })

	firstStop, err := StartFromEnv("catalog-api")
	if err != nil {
		t.Fatalf("first StartFromEnv: %v", err)
	}
	secondStop, err := StartFromEnv("catalog-api")
	if err != nil {
		t.Fatalf("second StartFromEnv: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("profiler starts = %d, want 1", startCalls)
	}
	firstStop()
	secondStop()
	if stopCalls != 1 {
		t.Fatalf("profiler stops = %d, want 1", stopCalls)
	}
	if _, err := StartFromEnv("another-service"); err == nil {
		t.Fatal("second service identity unexpectedly reused the process profiler")
	}
}

func TestLoadBasicAuthFromFleetSecrets(t *testing.T) {
	values := map[string]string{
		userSecretEnv:     "pyroscope-user",
		passwordSecretEnv: "pyroscope-password",
	}
	lookups := []string{}
	user, password, err := loadBasicAuth(func(name string) string { return values[name] }, func(name string) (string, error) {
		lookups = append(lookups, name)
		return map[string]string{"pyroscope-user": "profile-writer", "pyroscope-password": "secret-value"}[name], nil
	})
	if err != nil || user != "profile-writer" || password != "secret-value" {
		t.Fatalf("loadBasicAuth = (%q, %q), %v", user, password, err)
	}
	if len(lookups) != 2 || lookups[0] != "pyroscope-user" || lookups[1] != "pyroscope-password" {
		t.Fatalf("unexpected secret lookups: %#v", lookups)
	}
}

func TestLoadBasicAuthRejectsPartialAndMixedSources(t *testing.T) {
	tests := []map[string]string{
		{userSecretEnv: "only-user"},
		{userFileEnv: "/tmp/user-only"},
		{userSecretEnv: "vault-user", passwordSecretEnv: "vault-pass", userFileEnv: "/tmp/user", passwordFileEnv: "/tmp/pass"},
	}
	for _, values := range tests {
		if _, _, err := loadBasicAuth(func(name string) string { return values[name] }, nil); err == nil {
			t.Errorf("loadBasicAuth accepted invalid source combination: %#v", values)
		}
	}
}

func TestLoadBasicAuthHidesVaultError(t *testing.T) {
	values := map[string]string{userSecretEnv: "pyroscope-user", passwordSecretEnv: "pyroscope-password"}
	_, _, err := loadBasicAuth(func(name string) string { return values[name] }, func(string) (string, error) {
		return "", os.ErrPermission
	})
	if err == nil || strings.Contains(err.Error(), "permission") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("vault lookup error was not normalized: %v", err)
	}
}
