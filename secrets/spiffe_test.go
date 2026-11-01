package secrets

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resetSPIFFE clears the process-wide state so each test starts without a factory, legs or counters.
func resetSPIFFE(t *testing.T) {
	t.Helper()
	clear := func() {
		spiffeFactoryMu.Lock()
		spiffeFactory = nil
		spiffeFactoryMu.Unlock()
		spiffeLegsMu.Lock()
		spiffeLegs = map[string]*spiffeLeg{}
		spiffeLegsMu.Unlock()
		spiffeReads.Store(0)
		spiffeFallback.Store(0)
	}
	clear()
	t.Cleanup(clear)
}

type fakeSPIFFEClient struct {
	do     func(*http.Request) (*http.Response, error)
	closed atomic.Bool
}

func (f *fakeSPIFFEClient) Do(r *http.Request) (*http.Response, error) { return f.do(r) }
func (f *fakeSPIFFEClient) Close() error                               { f.closed.Store(true); return nil }

const spVaultBody = `{"success":true,"data":{"name":"s1","value":"the-value"}}`

// spVault answers GET /secrets/s1 over the key path (X-API-Key) and over the SPIFFE path (no key).
type spVault struct {
	*httptest.Server
	keyCalls, spiffeCalls atomic.Int64
	spiffeStatus          atomic.Int64 // 0 = 200
}

func newSPVault(t *testing.T) *spVault {
	v := &spVault{}
	v.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			if r.Header.Get("X-API-Key") != "good-key" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			v.keyCalls.Add(1)
		} else {
			v.spiffeCalls.Add(1)
			if st := int(v.spiffeStatus.Load()); st != 0 {
				http.Error(w, "refused", st)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(spVaultBody))
	}))
	t.Cleanup(v.Close)
	return v
}

// spiffeFactoryFor returns a factory whose client talks to v's TLS listener (standing in for the mTLS one).
func spiffeFactoryFor(v *spVault, calls *atomic.Int64, fail error) SPIFFEClientFactory {
	return func(_ context.Context, socket string, ids []string, _ time.Duration) (SPIFFEClient, error) {
		calls.Add(1)
		if fail != nil {
			return nil, fail
		}
		if socket != "unix:///run/spire/sockets/agent.sock" || len(ids) != 1 || ids[0] != DefaultSPIFFEServerID {
			return nil, errors.New("unexpected socket or server id")
		}
		hc := v.Client()
		hc.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		return &fakeSPIFFEClient{do: hc.Do}, nil
	}
}

func dualClient(t *testing.T, v *spVault, apiURL string) *Client {
	t.Helper()
	hc := v.Client()
	c := New(apiURL, "good-key", hc)
	env := map[string]string{
		"FLEET_SECRETS_SPIFFE":     "dual",
		"FLEET_SECRETS_SPIFFE_URL": v.URL,
		"SPIFFE_ENDPOINT_SOCKET":   "unix:///run/spire/sockets/agent.sock",
	}
	c.WithEnv(func(k string) string { return env[k] })
	if c.sp == nil {
		t.Fatal("SPIFFE path not enabled")
	}
	return c
}

// An API key read and a SPIFFE read return the same secret; the SPIFFE one sends no key.
func TestSPIFFEAndAPIKeyReadTheSameSecret(t *testing.T) {
	resetSPIFFE(t)
	v := newSPVault(t)
	var connects atomic.Int64
	SetSPIFFEClientFactory(spiffeFactoryFor(v, &connects, nil))

	keyOnly := New(v.URL, "good-key", v.Client())
	want, err := keyOnly.Get(context.Background(), "s1")
	if err != nil || want != "the-value" {
		t.Fatalf("key read = %q, %v", want, err)
	}

	c := dualClient(t, v, v.URL)
	res, err := c.GetDetailed(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Value != want || res.Source != SourceSPIFFE {
		t.Fatalf("spiffe read = %q via %q; want %q via spiffe", res.Value, res.Source, want)
	}
	if v.spiffeCalls.Load() != 1 || v.keyCalls.Load() != 1 {
		t.Fatalf("calls: spiffe=%d key=%d; want 1 and 1 (the key read above only)", v.spiffeCalls.Load(), v.keyCalls.Load())
	}
	if served, fb := SPIFFEStats(); served != 1 || fb != 0 {
		t.Fatalf("stats = %d, %d", served, fb)
	}
}

// Any refusal on the SPIFFE path sends the same Get to the API key, which answers.
func TestSPIFFERefusalFallsBackToTheKey(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		resetSPIFFE(t)
		v := newSPVault(t)
		v.spiffeStatus.Store(int64(status))
		var connects atomic.Int64
		SetSPIFFEClientFactory(spiffeFactoryFor(v, &connects, nil))
		c := dualClient(t, v, v.URL)
		res, err := c.GetDetailed(context.Background(), "s1")
		if err != nil || res.Value != "the-value" || res.Source != SourceLive {
			t.Fatalf("status %d: result %+v, %v; want the value over the key path", status, res, err)
		}
		if v.keyCalls.Load() != 1 {
			t.Fatalf("status %d: key calls = %d", status, v.keyCalls.Load())
		}
		if _, fb := SPIFFEStats(); fb != 1 {
			t.Fatalf("status %d: fallback count = %d", status, fb)
		}
	}
}

// A path failure (5xx) starts a cool-down: the next reads skip SPIFFE; a refusal (403) does not.
func TestSPIFFEFailureCooldown(t *testing.T) {
	resetSPIFFE(t)
	v := newSPVault(t)
	var connects atomic.Int64
	SetSPIFFEClientFactory(spiffeFactoryFor(v, &connects, nil))
	c := dualClient(t, v, v.URL)

	v.spiffeStatus.Store(http.StatusForbidden)
	for i := 0; i < 2; i++ {
		if _, err := c.Get(context.Background(), "s1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := v.spiffeCalls.Load(); got != 2 {
		t.Fatalf("after two 403s the SPIFFE path was tried %d times; want 2 (a refusal is no cool-down)", got)
	}

	v.spiffeStatus.Store(http.StatusServiceUnavailable)
	for i := 0; i < 3; i++ {
		if _, err := c.Get(context.Background(), "s1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := v.spiffeCalls.Load(); got != 3 {
		t.Fatalf("SPIFFE calls = %d; want 3 (one 503, then the cool-down skips it)", got)
	}
}

// No factory, a factory that cannot connect, or the mode off: the key path, unchanged.
func TestSPIFFEUnavailableUsesTheKey(t *testing.T) {
	cases := map[string]func(*spVault){
		"no factory": func(*spVault) {},
		"connect error": func(v *spVault) {
			var n atomic.Int64
			SetSPIFFEClientFactory(spiffeFactoryFor(v, &n, errors.New("no workload api")))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			resetSPIFFE(t)
			v := newSPVault(t)
			setup(v)
			c := dualClient(t, v, v.URL)
			res, err := c.GetDetailed(context.Background(), "s1")
			if err != nil || res.Value != "the-value" || res.Source != SourceLive {
				t.Fatalf("result %+v, %v", res, err)
			}
			if v.spiffeCalls.Load() != 0 {
				t.Fatal("the SPIFFE path reached the vault without a client")
			}
		})
	}
	t.Run("off", func(t *testing.T) {
		resetSPIFFE(t)
		v := newSPVault(t)
		var n atomic.Int64
		SetSPIFFEClientFactory(spiffeFactoryFor(v, &n, nil))
		c := New(v.URL, "good-key", v.Client())
		env := map[string]string{"FLEET_SECRETS_SPIFFE": "off", "FLEET_SECRETS_SPIFFE_URL": v.URL, "SPIFFE_ENDPOINT_SOCKET": "unix:///x"}
		c.WithEnv(func(k string) string { return env[k] })
		if c.sp != nil {
			t.Fatal("mode off enabled the path")
		}
		if _, err := c.Get(context.Background(), "s1"); err != nil || n.Load() != 0 {
			t.Fatalf("err=%v connects=%d", err, n.Load())
		}
	})
}

// The first read waits for the Workload API connect, so a boot-time read is not sent on the key.
func TestSPIFFEFirstReadWaitsForTheConnect(t *testing.T) {
	resetSPIFFE(t)
	v := newSPVault(t)
	hc := v.Client()
	hc.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	SetSPIFFEClientFactory(func(context.Context, string, []string, time.Duration) (SPIFFEClient, error) {
		time.Sleep(200 * time.Millisecond)
		return &fakeSPIFFEClient{do: hc.Do}, nil
	})
	c := dualClient(t, v, v.URL)
	res, err := c.GetDetailed(context.Background(), "s1")
	if err != nil || res.Source != SourceSPIFFE {
		t.Fatalf("result %+v, %v; want a SPIFFE read", res, err)
	}
}

func TestSPIFFEEnvValidation(t *testing.T) {
	resetSPIFFE(t)
	good := map[string]string{"FLEET_SECRETS_SPIFFE": "dual", "FLEET_SECRETS_SPIFFE_URL": "https://go-fleet-secrets:8443", "SPIFFE_ENDPOINT_SOCKET": "unix:///run/spire/sockets/agent.sock"}
	with := func(over map[string]string) *Client {
		env := map[string]string{}
		for k, v := range good {
			env[k] = v
		}
		for k, v := range over {
			env[k] = v
		}
		return New("https://fleet-secrets.0exec.com", "k", http.DefaultClient).WithEnv(func(k string) string { return env[k] })
	}
	if c := with(nil); c.sp == nil || c.sp.base != "https://go-fleet-secrets:8443" || c.sp.serverID != DefaultSPIFFEServerID {
		t.Fatalf("good config not applied: %+v", c.sp)
	}
	for name, over := range map[string]map[string]string{
		"http":        {"FLEET_SECRETS_SPIFFE_URL": "http://go-fleet-secrets:8443"},
		"credentials": {"FLEET_SECRETS_SPIFFE_URL": "https://u:p@go-fleet-secrets:8443"},
		"path":        {"FLEET_SECRETS_SPIFFE_URL": "https://go-fleet-secrets:8443/secrets"},
		"query":       {"FLEET_SECRETS_SPIFFE_URL": "https://go-fleet-secrets:8443/?a=b"},
		"empty":       {"FLEET_SECRETS_SPIFFE_URL": ""},
		"bad id":      {"FLEET_SECRETS_SPIFFE_SERVER_ID": "fleet-secrets"},
		"no socket":   {"SPIFFE_ENDPOINT_SOCKET": ""},
		"tcp socket":  {"SPIFFE_ENDPOINT_SOCKET": "tcp://127.0.0.1:1"},
		"mode typo":   {"FLEET_SECRETS_SPIFFE": "true"},
	} {
		if c := with(over); c.sp != nil {
			t.Errorf("%s: path enabled for an invalid configuration", name)
		}
	}
	if c := with(map[string]string{"FLEET_SECRETS_SPIFFE_URL": "HTTPS://Go-Fleet-Secrets:8443"}); c.sp == nil || strings.ToLower(c.sp.base) != c.sp.base {
		t.Error("URL not normalised")
	}
}
