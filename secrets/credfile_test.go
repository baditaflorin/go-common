package secrets

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/baditaflorin/go-common/credfile"
)

// keyVault answers like the vault: 200 with an envelope to an accepted key,
// 401 to any other, and records the key of every request.
type keyVault struct {
	mu   sync.Mutex
	good map[string]bool
	seen []string
}

func (v *keyVault) Do(req *http.Request) (*http.Response, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := req.Header.Get("X-API-Key")
	v.seen = append(v.seen, key)
	if !v.good[key] {
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"status":"error"}`)), Header: http.Header{}}, nil
	}
	body := `{"status":"success","data":{"name":"s","value":"vault-value"}}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func (v *keyVault) accept(keys ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.good = map[string]bool{}
	for _, k := range keys {
		v.good[k] = true
	}
}

func (v *keyVault) requests() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.seen...)
}

func writeKeyFile(t *testing.T, path, value string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func fileEnv(path string, extra map[string]string) func(string) string {
	return func(k string) string {
		if k == "FLEET_SECRETS_API_KEY_FILE" {
			return path
		}
		return extra[k]
	}
}

func TestFileKeyRenewalPickedUpOn401(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet_secrets_api_key")
	writeKeyFile(t, path, "ak_vault_old")
	vault := &keyVault{}
	vault.accept("ak_vault_old")
	c, err := NewFromEnv(fileEnv(path, nil), vault)
	if err != nil {
		t.Fatal(err)
	}
	if c.keySrc == nil {
		t.Fatal("the key file is not followed")
	}
	if _, err := c.Get(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}

	// The renewal lands and the old key stops working before the next check.
	writeKeyFile(t, path, "ak_vault_new")
	vault.accept("ak_vault_new")
	got, err := c.Get(context.Background(), "s")
	if err != nil || got != "vault-value" {
		t.Fatalf("Get after renewal = %q, %v", got, err)
	}
	if seen := vault.requests(); len(seen) != 3 || seen[1] != "ak_vault_old" || seen[2] != "ak_vault_new" {
		t.Fatalf("vault saw %q", seen)
	}

	// A key the vault refuses, with nothing better in the file: one request,
	// no retry, the 401 surfaces as before.
	vault.accept()
	_, err = c.Get(context.Background(), "s")
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refused key: %v", err)
	}
	if seen := vault.requests(); len(seen) != 4 {
		t.Fatalf("a refused unchanged key was retried: %q", seen)
	}
}

func TestFileKeyMissingAfterStartKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet_secrets_api_key")
	writeKeyFile(t, path, "ak_vault_steady")
	vault := &keyVault{}
	vault.accept("ak_vault_steady")
	c, err := NewFromEnv(fileEnv(path, nil), vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Force a look at the file through the 401 path's reader.
	c.keySrc.Rejected("not-the-key")
	if v, err := c.Get(context.Background(), "s"); err != nil || v != "vault-value" {
		t.Fatalf("Get with the key file gone = %q, %v", v, err)
	}
	if st := c.keySrc.Stats(); !st.Stale || st.Errors[credfile.ReasonMissing] == 0 || st.StaleUses == 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// TestEnvKeyNotFollowed: FLEET_SECRETS_API_KEY and FLEET_API_KEY behave as
// before: no file is followed, even when one is configured.
func TestEnvKeyNotFollowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet_secrets_api_key")
	writeKeyFile(t, path, "ak_file")
	c, err := NewFromEnv(fileEnv(path, map[string]string{"FLEET_SECRETS_API_KEY": "ak_env"}), &keyVault{})
	if err != nil || c.keySrc != nil || c.apiKey != "ak_env" {
		t.Fatalf("env key: err=%v followed=%v", err, c != nil && c.keySrc != nil)
	}
	c, err = NewFromEnv(func(k string) string {
		if k == "FLEET_API_KEY" {
			return "ak_fleet"
		}
		return ""
	}, &keyVault{})
	if err != nil || c.keySrc != nil || c.apiKey != "ak_fleet" {
		t.Fatalf("FLEET_API_KEY: err=%v", err)
	}
}

func TestFileKeyReloadOff(t *testing.T) {
	t.Setenv(credfile.ReloadEnv, "off")
	path := filepath.Join(t.TempDir(), "fleet_secrets_api_key")
	writeKeyFile(t, path, "ak_vault_start")
	vault := &keyVault{}
	c, err := NewFromEnv(fileEnv(path, nil), vault)
	if err != nil {
		t.Fatal(err)
	}
	writeKeyFile(t, path, "ak_vault_later")
	vault.accept("ak_vault_later")
	if _, err := c.Get(context.Background(), "s"); err == nil {
		t.Fatal("reload off picked up the new key")
	}
	if seen := vault.requests(); len(seen) != 1 || seen[0] != "ak_vault_start" {
		t.Fatalf("vault saw %q", seen)
	}
}
