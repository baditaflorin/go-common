package telemetry

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/baditaflorin/go-common/secrets"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func renewVaultKeyFile(t *testing.T, path, value string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value+"\n"), 0o400); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Error(err)
	}
}

// TestStartTime401ReadsTheRenewedKeyFileOnce: the vault refuses the key read
// at start because a renewal replaced it in the meantime; Init's read sends
// the new key once and gets the credential.
func TestStartTime401ReadsTheRenewedKeyFileOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet_secrets_api_key")
	renewVaultKeyFile(t, path, "ak_vault_before")
	values := map[string]string{"FLEET_SECRETS_API_KEY_FILE": path}
	var keys []string
	cfg := &Config{SampleRate: 0.1}
	err := configureOpenObserveFromFleetSecrets(cfg, func(k string) string { return values[k] }, func(_ context.Context, _, apiKey string) (string, error) {
		keys = append(keys, apiKey)
		if apiKey == "ak_vault_before" {
			renewVaultKeyFile(t, path, "ak_vault_after")
			return "", &secrets.StatusError{Name: openObserveTokenSecret, StatusCode: http.StatusUnauthorized}
		}
		return "o2_token", nil
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if len(keys) != 2 || keys[1] != "ak_vault_after" {
		t.Fatalf("vault reads used %d keys, the second renewed=%v", len(keys), len(keys) == 2 && keys[1] == "ak_vault_after")
	}
	if cfg.otlpHeaders["Authorization"] == "" {
		t.Fatal("the credential read with the renewed key was not applied")
	}
}

// TestStartTime401WithoutRenewalIsNotRetried: nothing better in the file, so
// one read and the usual fail-closed error.
func TestStartTime401WithoutRenewalIsNotRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet_secrets_api_key")
	renewVaultKeyFile(t, path, "ak_vault_refused")
	values := map[string]string{"FLEET_SECRETS_API_KEY_FILE": path}
	reads := 0
	err := configureOpenObserveFromFleetSecrets(&Config{}, func(k string) string { return values[k] }, func(context.Context, string, string) (string, error) {
		reads++
		return "", &secrets.StatusError{Name: openObserveTokenSecret, StatusCode: http.StatusUnauthorized}
	})
	if err == nil || reads != 1 {
		t.Fatalf("err=%v reads=%d; want the error after one read", err, reads)
	}
}

// TestBackgroundRetryFollowsTheKeyFile: Init's read fails transiently, the
// first retry is refused (the key was renewed in between), the next retry
// sends the renewed key and brings tracing up.
func TestBackgroundRetryFollowsTheKeyFile(t *testing.T) {
	isolateProviderState(t)
	path := filepath.Join(t.TempDir(), "fleet_secrets_api_key")
	renewVaultKeyFile(t, path, "ak_vault_one")
	env := map[string]string{"FLEET_SECRETS_API_KEY_FILE": path}

	var mu sync.Mutex
	var keys []string
	errs, logs := newSink(), newSink()
	proc := &countingProcessor{}
	deps := initDeps{
		getenv: func(k string) string { return env[k] },
		readToken: func(_ context.Context, _, apiKey string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			keys = append(keys, apiKey)
			switch len(keys) {
			case 1:
				return "", &secrets.StatusError{Name: openObserveTokenSecret, StatusCode: http.StatusServiceUnavailable}
			case 2:
				renewVaultKeyFile(t, path, "ak_vault_two")
				return "", &secrets.StatusError{Name: openObserveTokenSecret, StatusCode: http.StatusUnauthorized}
			}
			if apiKey != "ak_vault_two" {
				return "", &secrets.StatusError{Name: openObserveTokenSecret, StatusCode: http.StatusUnauthorized}
			}
			return "o2_token", nil
		},
		newProvider: func(*Config) (*sdktrace.TracerProvider, error) { return newCountedProvider(proc), nil },
		retry:       fastPolicy(),
		logf:        logs.logf,
		handle:      errs.handle,
	}
	cfg := initWith(deps, "svc", "1.0.0")
	if cfg.retry == nil {
		t.Fatal("a transient start-time failure did not start the retry")
	}
	awaitDone(t, cfg.retry)
	if installedProvider() == nil {
		t.Fatalf("tracing did not come up; errors: %q", errs.all())
	}
	mu.Lock()
	got := append([]string(nil), keys...)
	mu.Unlock()
	if len(got) != 3 || got[0] != "ak_vault_one" || got[1] != "ak_vault_one" || got[2] != "ak_vault_two" {
		t.Fatalf("reads used %d keys; want old, old, renewed", len(got))
	}
	for _, line := range append(errs.all(), logs.all()...) {
		if strings.Contains(line, "ak_vault_") {
			t.Fatalf("a key reached the output: %q", line)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := cfg.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	awaitNoRetryGoroutine(t)
}
