package fleetfetch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/header"
)

type fakeIdentity struct {
	mu    sync.Mutex
	calls int
	aud   string
	life  time.Duration
	err   error
	n     int
}

func (f *fakeIdentity) Token(_ context.Context, audience string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.aud = audience
	if f.err != nil {
		return "", f.err
	}
	f.n++
	return fakeJWT(time.Now().Add(f.life), f.n), nil
}

func fakeJWT(exp time.Time, n int) string {
	enc := base64.RawURLEncoding.EncodeToString
	return fmt.Sprintf("eyJhbGciOiJFUzI1NiJ9.%s.%s", enc([]byte(fmt.Sprintf(`{"exp":%d,"n":%d}`, exp.Unix(), n))), enc([]byte("sig")))
}

type cacheSeen struct {
	key, jwt, caller string
}

func newIdentityCache(t *testing.T, seen *atomic.Pointer[cacheSeen]) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(&cacheSeen{key: r.Header.Get(header.APIKey), jwt: r.Header.Get(SPIFFEJWTHeader), caller: r.Header.Get(header.FleetCaller)})
		w.Header().Set("X-FetchCache-Hit", "false")
		w.Header().Set("X-FetchCache-Fetched-At", time.Now().UTC().Format(time.RFC3339))
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Off by default: no identity configured sends no JWT-SVID header and emits no Identity.
func TestIdentityOffByDefault(t *testing.T) {
	SetDefaultIdentity(nil, "")
	obs := &captureObserver{}
	SetDefaultObserver(obs)
	defer SetDefaultObserver(nil)
	var seen atomic.Pointer[cacheSeen]
	cache := newIdentityCache(t, &seen)
	c := NewClient(WithCacheURL(cache.URL), WithAPIKey("ak_test_key"))
	if _, err := c.Get(context.Background(), "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if s := seen.Load(); s.jwt != "" || s.key != "ak_test_key" {
		t.Fatalf("saw jwt=%q key=%q, want no jwt and the key", s.jwt, s.key)
	}
	if len(obs.events) != 1 || obs.events[0].Identity != "" {
		t.Fatalf("events %+v: want one with empty Identity", obs.events)
	}
}

// DUAL MODE: the JWT-SVID goes NEXT TO the key, for the configured audience.
func TestIdentitySendsJWTNextToKey(t *testing.T) {
	SetDefaultIdentity(nil, "")
	obs := &captureObserver{}
	SetDefaultObserver(obs)
	defer SetDefaultObserver(nil)
	var seen atomic.Pointer[cacheSeen]
	cache := newIdentityCache(t, &seen)
	src := &fakeIdentity{life: 5 * time.Minute}
	c := NewClient(WithCacheURL(cache.URL), WithAPIKey("ak_test_key"), WithIdentity(src, ""))
	for i := 0; i < 3; i++ {
		if _, err := c.Get(context.Background(), fmt.Sprintf("https://example.com/%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	s := seen.Load()
	if s.key != "ak_test_key" {
		t.Fatalf("key %q was not sent next to the JWT-SVID", s.key)
	}
	if s.jwt == "" {
		t.Fatal("no JWT-SVID header")
	}
	if src.aud != DefaultSPIFFEAudience {
		t.Fatalf("audience %q, want the default %q", src.aud, DefaultSPIFFEAudience)
	}
	if src.calls != 1 {
		t.Fatalf("token minted %d times for 3 calls inside one token's life, want 1", src.calls)
	}
	for _, e := range obs.events {
		if e.Identity != IdentitySPIFFE {
			t.Fatalf("event %+v: want identity %q", e, IdentitySPIFFE)
		}
	}
}

func TestIdentityAudienceFromEnvAndOption(t *testing.T) {
	SetDefaultIdentity(nil, "")
	t.Setenv(EnvSPIFFEAudience, "spiffe://0exec.com/ns/fleet/sa/other-cache")
	src := &fakeIdentity{life: 5 * time.Minute}
	id := newIdentity(src, "")
	if id.audience != "spiffe://0exec.com/ns/fleet/sa/other-cache" {
		t.Fatalf("env audience: %q", id.audience)
	}
	if id := newIdentity(src, " spiffe://explicit "); id.audience != "spiffe://explicit" {
		t.Fatalf("explicit audience: %q", id.audience)
	}
}

// No JWT-SVID can be had (Workload API down, no entry): the call goes on its key
// alone, is counted "unavailable", and the source is not hammered.
func TestIdentityUnavailableFallsBackToKeyAndBacksOff(t *testing.T) {
	SetDefaultIdentity(nil, "")
	obs := &captureObserver{}
	SetDefaultObserver(obs)
	defer SetDefaultObserver(nil)
	var seen atomic.Pointer[cacheSeen]
	cache := newIdentityCache(t, &seen)
	src := &fakeIdentity{err: errors.New("workload api down")}
	c := NewClient(WithCacheURL(cache.URL), WithAPIKey("ak_test_key"), WithIdentity(src, ""))
	for i := 0; i < 5; i++ {
		res, err := c.Get(context.Background(), fmt.Sprintf("https://example.com/%d", i))
		if err != nil || res.Status != 200 {
			t.Fatalf("call %d: %v %+v", i, err, res)
		}
	}
	if s := seen.Load(); s.jwt != "" || s.key != "ak_test_key" {
		t.Fatalf("saw jwt=%q key=%q, want the key alone", s.jwt, s.key)
	}
	if src.calls != 1 {
		t.Fatalf("source asked %d times inside the back-off, want 1", src.calls)
	}
	for _, e := range obs.events {
		if e.Identity != IdentityUnavailable {
			t.Fatalf("event %+v: want identity %q", e, IdentityUnavailable)
		}
	}
}

// A token with little life left is replaced; a failing source does not drop a token that is still valid.
func TestIdentityRefreshesNearExpiryAndKeepsValidTokenOnFailure(t *testing.T) {
	src := &fakeIdentity{life: time.Minute} // under identityMinLife: asked again every call
	id := newIdentity(src, "")
	first := id.jwt(context.Background())
	if first == "" {
		t.Fatal("no token")
	}
	// the next call finds under 90 s left, but the retry window is not open after a success: it asks again
	second := id.jwt(context.Background())
	if second == first || src.calls != 2 {
		t.Fatalf("near-expiry token was not replaced (calls=%d)", src.calls)
	}
	src.mu.Lock()
	src.err = errors.New("down")
	src.mu.Unlock()
	if got := id.jwt(context.Background()); got != second {
		t.Fatalf("a still-valid token was dropped on a refresh failure")
	}
	if got := id.jwt(context.Background()); got != second || src.calls != 3 {
		t.Fatalf("inside the back-off: want the cached token and no new ask (calls=%d)", src.calls)
	}
}

func TestIdentityRejectsUnusableTokens(t *testing.T) {
	for name, tok := range map[string]string{"empty": "", "garbage": "abc", "no exp": "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c", "expired": fakeJWT(time.Now().Add(-time.Minute), 1)} {
		id := newIdentity(staticIdentity(tok), "")
		if got := id.jwt(context.Background()); got != "" {
			t.Errorf("%s: token %q was used", name, got)
		}
	}
}

type staticIdentity string

func (s staticIdentity) Token(context.Context, string) (string, error) { return string(s), nil }

// The process default is used by a client that has no source of its own.
func TestDefaultIdentityAndOverride(t *testing.T) {
	defer SetDefaultIdentity(nil, "")
	var seen atomic.Pointer[cacheSeen]
	cache := newIdentityCache(t, &seen)
	SetDefaultIdentity(&fakeIdentity{life: 5 * time.Minute}, "")
	c := NewClient(WithCacheURL(cache.URL), WithAPIKey("k1234567890"))
	if _, err := c.Get(context.Background(), "https://example.com/a"); err != nil {
		t.Fatal(err)
	}
	if seen.Load().jwt == "" {
		t.Fatal("process default identity not used")
	}
}

// The JWT-SVID is for the cache hop only: the direct-to-origin fallback never carries it.
func TestIdentityNotSentToOriginOnFallback(t *testing.T) {
	SetDefaultIdentity(nil, "")
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }))
	defer cacheSrv.Close()
	var originJWT atomic.Value
	originJWT.Store("unset")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originJWT.Store(r.Header.Get(SPIFFEJWTHeader))
		w.WriteHeader(200)
	}))
	defer origin.Close()
	c := NewClient(WithCacheURL(cacheSrv.URL), WithAPIKey("k1234567890"), WithIdentity(&fakeIdentity{life: 5 * time.Minute}, ""),
		WithFallbackClient(&http.Client{Timeout: 3 * time.Second}))
	if _, err := c.Get(context.Background(), origin.URL); err != nil {
		t.Fatal(err)
	}
	if got := originJWT.Load().(string); got != "" {
		t.Fatalf("origin saw X-Spiffe-Jwt %q", got)
	}
}
