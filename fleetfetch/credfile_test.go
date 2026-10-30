package fleetfetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/credfile"
	"github.com/baditaflorin/go-common/header"
)

func replaceKeyFile(t *testing.T, path, value string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// keyCache answers 200 to the keys in good and 401 to every other, and
// records the key of every request.
type keyCache struct {
	mu   sync.Mutex
	good map[string]bool
	seen []string
}

func (k *keyCache) handler(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := r.Header.Get(header.APIKey)
	k.seen = append(k.seen, key)
	if !k.good[key] {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("X-FetchCache-Hit", "true")
	_, _ = w.Write([]byte("ok"))
}

func (k *keyCache) setGood(keys ...string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.good = map[string]bool{}
	for _, key := range keys {
		k.good[key] = true
	}
}

func (k *keyCache) requests() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.seen...)
}

func fileKeyClient(t *testing.T, opts credfile.Options, first string) (*Client, string, *keyCache) {
	t.Helper()
	prev := apiKeyFileOptions
	apiKeyFileOptions = opts
	t.Cleanup(func() { apiKeyFileOptions = prev })
	keyPath := filepath.Join(t.TempDir(), "fleet_fetch_cache_api_key")
	replaceKeyFile(t, keyPath, first)
	t.Setenv(EnvAPIKey, "")
	t.Setenv(EnvAPIKeyFile, keyPath)
	kc := &keyCache{}
	srv := httptest.NewServer(http.HandlerFunc(kc.handler))
	t.Cleanup(srv.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an auth failure fell back to a direct fetch")
	}))
	t.Cleanup(origin.Close)
	c := NewClient(WithCacheURL(srv.URL), WithFallbackClient(origin.Client()))
	if c.apiKeySrc == nil {
		t.Fatal("a file-backed key is not followed")
	}
	return c, keyPath, kc
}

// TestFileKeyRenewedPickedUpOn401: the renewal lands between two checks; the
// cache refuses the old key, the client reads the file once, retries once with
// the new key and succeeds.
func TestFileKeyRenewedPickedUpOn401(t *testing.T) {
	c, keyPath, kc := fileKeyClient(t, credfile.Options{MaxBytes: 1 << 20, CheckInterval: time.Minute}, "ak_old_key")
	kc.setGood("ak_old_key")
	if _, err := c.Get(context.Background(), "https://example.com/a"); err != nil {
		t.Fatal(err)
	}

	replaceKeyFile(t, keyPath, "ak_new_key")
	kc.setGood("ak_new_key") // the keystore no longer accepts the old one
	r, err := c.Get(context.Background(), "https://example.com/b")
	if err != nil {
		t.Fatalf("Get after renewal: %v", err)
	}
	if string(r.Body) != "ok" {
		t.Fatalf("body = %q", r.Body)
	}
	got := c.Stats()
	if got.Errors != 0 {
		t.Fatalf("stats = %+v", got)
	}
	want := []string{"ak_old_key", "ak_old_key", "ak_new_key"}
	if seen := kc.requests(); len(seen) != len(want) || seen[1] != want[1] || seen[2] != want[2] {
		t.Fatalf("cache saw %q, want %q", seen, want)
	}
	// Later requests use the new key directly.
	if _, err := c.Get(context.Background(), "https://example.com/c"); err != nil {
		t.Fatal(err)
	}
	if seen := kc.requests(); seen[len(seen)-1] != "ak_new_key" || len(seen) != 4 {
		t.Fatalf("cache saw %q", seen)
	}
}

// TestFileKey401NoLoop: a refused key with nothing better in the file is sent
// once; a renewed key the cache also refuses is tried once; then the circuit
// is open as before, until the file changes.
func TestFileKey401NoLoop(t *testing.T) {
	c, keyPath, kc := fileKeyClient(t, credfile.Options{MaxBytes: 1 << 20, CheckInterval: time.Nanosecond, RejectInterval: time.Nanosecond}, "ak_bad_one")
	kc.setGood()

	_, err := c.Get(context.Background(), "https://example.com/a")
	var authErr *CacheAuthError
	if !errors.As(err, &authErr) || authErr.CircuitOpen {
		t.Fatalf("first Get = %v", err)
	}
	if n := len(kc.requests()); n != 1 {
		t.Fatalf("an unchanged refused key was sent %d times", n)
	}
	_, err = c.Get(context.Background(), "https://example.com/b")
	if !errors.As(err, &authErr) || !authErr.CircuitOpen || len(kc.requests()) != 1 {
		t.Fatalf("circuit not open with an unchanged file: %v, %d requests", err, len(kc.requests()))
	}

	// A new key appears: the circuit closes, it is tried; refused too, it is
	// not retried again and the circuit opens on it.
	replaceKeyFile(t, keyPath, "ak_bad_two")
	_, err = c.Get(context.Background(), "https://example.com/c")
	if !errors.As(err, &authErr) || authErr.CircuitOpen {
		t.Fatalf("Get after the file changed = %v", err)
	}
	if seen := kc.requests(); len(seen) != 2 || seen[1] != "ak_bad_two" {
		t.Fatalf("cache saw %q", seen)
	}

	// Once the file holds a key the cache accepts, requests succeed again,
	// without a restart.
	replaceKeyFile(t, keyPath, "ak_good")
	kc.setGood("ak_good")
	if _, err := c.Get(context.Background(), "https://example.com/d"); err != nil {
		t.Fatalf("Get with a good renewed key: %v", err)
	}
}

// TestFileKeyMissingKeepsLastGood: the key file disappears (a renewal writer
// between two renames, a remount); requests keep the last good key.
func TestFileKeyMissingKeepsLastGood(t *testing.T) {
	c, keyPath, kc := fileKeyClient(t, credfile.Options{MaxBytes: 1 << 20, CheckInterval: time.Nanosecond}, "ak_steady")
	kc.setGood("ak_steady")
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "https://example.com/a"); err != nil {
		t.Fatalf("Get with the key file missing: %v", err)
	}
	if st := c.apiKeySrc.Stats(); !st.Stale || st.StaleUses == 0 || st.Errors[credfile.ReasonMissing] == 0 {
		t.Fatalf("credfile stats = %+v", st)
	}
}

// TestEnvKeyUnchanged: a key from the environment is not followed, and a 401
// opens the circuit for good, as before.
func TestEnvKeyUnchanged(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "fleet_fetch_cache_api_key")
	replaceKeyFile(t, keyPath, "ak_file")
	t.Setenv(EnvAPIKey, "ak_env")
	t.Setenv(EnvAPIKeyFile, keyPath)
	c := NewClient()
	if c.apiKeySrc != nil || c.apiKey != "ak_env" {
		t.Fatalf("env key: src=%v key from env=%v", c.apiKeySrc, c.apiKey == "ak_env")
	}
	t.Setenv(EnvAPIKey, "")
	t.Setenv(EnvAPIKeyFile, "")
	if c := NewClient(WithAPIKey("ak_opt")); c.apiKeySrc != nil {
		t.Fatal("WithAPIKey is followed from a file")
	}
}

// TestFileKeyReloadOff: FLEET_CREDFILE_RELOAD=off keeps the start-time key.
func TestFileKeyReloadOff(t *testing.T) {
	t.Setenv(credfile.ReloadEnv, "off")
	c, keyPath, kc := fileKeyClient(t, credfile.Options{MaxBytes: 1 << 20, CheckInterval: time.Nanosecond, RejectInterval: time.Nanosecond}, "ak_start")
	replaceKeyFile(t, keyPath, "ak_later")
	kc.setGood("ak_later")
	_, err := c.Get(context.Background(), "https://example.com/a")
	if !errors.Is(err, ErrCacheAuth) {
		t.Fatalf("reload off: Get = %v, want the old behaviour (auth error)", err)
	}
	if seen := kc.requests(); len(seen) != 1 || seen[0] != "ak_start" {
		t.Fatalf("cache saw %q", seen)
	}
}
