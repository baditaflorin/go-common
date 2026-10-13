package secrets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func vaultServer(t *testing.T, status int, value string, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"name":"x","value":"` + value + `"}}`))
	}))
}

func TestFallsOverOnOutageButNotOnDefinitiveAnswers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		primary    int
		wantStatus int // 0 = served by the fallback
	}{
		{"500 falls over", 500, 0}, {"503 falls over", 503, 0}, {"429 falls over", 429, 0},
		{"401 is final", 401, 401}, {"403 is final", 403, 403}, {"404 is final", 404, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h1, h2 int32
			a, b := vaultServer(t, tc.primary, "", &h1), vaultServer(t, 200, "from-follower", &h2)
			defer a.Close()
			defer b.Close()
			res, err := New(a.URL, "k", a.Client()).WithFallbacks(b.URL).GetDetailed(context.Background(), "x")
			if tc.wantStatus == 0 {
				if err != nil || res.Value != "from-follower" || res.Source != SourceAlt || res.URL != b.URL {
					t.Fatalf("res=%+v err=%v", res, err)
				}
				return
			}
			var se *StatusError
			if !errors.As(err, &se) || se.StatusCode != tc.wantStatus || atomic.LoadInt32(&h2) != 0 {
				t.Fatalf("want final %d without asking the follower; err=%v follower hits=%d", tc.wantStatus, err, h2)
			}
		})
	}
}

func TestFallsOverOnTransportError(t *testing.T) {
	var h int32
	b := vaultServer(t, 200, "v", &h)
	defer b.Close()
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close() // connection refused
	res, err := New(deadURL, "k", http.DefaultClient).WithFallbacks(b.URL).GetDetailed(context.Background(), "x")
	if err != nil || res.Value != "v" || res.Source != SourceAlt {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestCacheServesWhenEveryURLIsDownAndIsEncrypted(t *testing.T) {
	dir := t.TempDir()
	var h int32
	up := vaultServer(t, 200, "s3cret-value", &h)
	c := New(up.URL, "key-A", up.Client()).WithCache(dir, time.Hour)
	if res, err := c.GetDetailed(context.Background(), "tok"); err != nil || res.Source != SourceLive {
		t.Fatalf("first read: %+v %v", res, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tok.enc"))
	if err != nil || strings.Contains(string(raw), "s3cret-value") {
		t.Fatalf("cache file missing or plaintext: err=%v", err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "tok.enc")); st.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode %v", st.Mode().Perm())
	}
	up.Close() // vault is now down
	res, err := New(up.URL, "key-A", http.DefaultClient).WithCache(dir, time.Hour).GetDetailed(context.Background(), "tok")
	if err != nil || res.Value != "s3cret-value" || res.Source != SourceCache {
		t.Fatalf("outage read: %+v %v", res, err)
	}
	// a different API key cannot read the cache
	if _, err := New(up.URL, "key-B", http.DefaultClient).WithCache(dir, time.Hour).Get(context.Background(), "tok"); err == nil {
		t.Fatal("cache decrypted under the wrong key")
	}
}

func TestCacheIsNeverUsedForDefinitiveAnswersOrWhenStale(t *testing.T) {
	dir := t.TempDir()
	var h int32
	ok := vaultServer(t, 200, "v", &h)
	if _, err := New(ok.URL, "k", ok.Client()).WithCache(dir, time.Hour).Get(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	ok.Close()
	// 404 (the secret was deleted) must not resurrect the cached value
	gone := vaultServer(t, 404, "", &h)
	defer gone.Close()
	if _, err := New(gone.URL, "k", gone.Client()).WithCache(dir, time.Hour).Get(context.Background(), "tok"); err == nil {
		t.Fatal("cache resurrected a deleted secret")
	}
	// older than max age: ignored during an outage
	time.Sleep(15 * time.Millisecond)
	if _, err := New(ok.URL, "k", http.DefaultClient).WithCache(dir, time.Millisecond).Get(context.Background(), "tok"); err == nil {
		t.Fatal("stale cache entry served")
	}
}

func TestCacheNeverUsedWithoutAKey(t *testing.T) {
	dir := t.TempDir()
	c := New("http://127.0.0.1:1", "", http.DefaultClient).WithCache(dir, time.Hour)
	c.cache.store("tok", "v", "")
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("cache written under an empty key")
	}
}

func TestContextDeadlineStopsTheLoopAndUsesTheCache(t *testing.T) {
	dir := t.TempDir()
	var h1, h2 int32
	ok := vaultServer(t, 200, "v", &h1)
	if _, err := New(ok.URL, "k", ok.Client()).WithCache(dir, time.Hour).Get(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&h2, 1); <-r.Context().Done() }))
	defer slow.Close()
	other := vaultServer(t, 200, "other", &h1)
	defer other.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	res, err := New(slow.URL, "k", slow.Client()).WithFallbacks(other.URL).WithCache(dir, time.Hour).GetDetailed(ctx, "tok")
	if err != nil || res.Source != SourceCache {
		t.Fatalf("want cache after the deadline, got %+v %v", res, err)
	}
}

func TestWithEnvAllowlistsFallbackHosts(t *testing.T) {
	env := map[string]string{
		"FLEET_SECRETS_FALLBACK_URLS": "https://fleet-secrets.0mcp.com, https://evil.example, http://fleet-secrets.0own.com, https://fleet-secrets-standby.0own.com/x, https://fleet-secrets-standby.0own.com",
		"FLEET_SECRETS_CACHE_DIR":     "/tmp/x", "FLEET_SECRETS_CACHE_MAX_AGE": "2h",
	}
	c := New("https://fleet-secrets.0exec.com", "k", http.DefaultClient).WithEnv(func(k string) string { return env[k] })
	if len(c.fallbacks) != 2 || c.fallbacks[0] != "https://fleet-secrets.0mcp.com" || c.fallbacks[1] != "https://fleet-secrets-standby.0own.com" {
		t.Fatalf("fallbacks = %v", c.fallbacks)
	}
	if c.cache == nil || c.cache.maxAge != 2*time.Hour {
		t.Fatalf("cache = %+v", c.cache)
	}
	if bare := New("https://fleet-secrets.0exec.com", "k", http.DefaultClient).WithEnv(func(string) string { return "" }); len(bare.fallbacks) != 0 || bare.cache != nil {
		t.Fatal("WithEnv must be a no-op with nothing set")
	}
}
