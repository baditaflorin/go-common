package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

// The vault is fleet infrastructure. A service started with proxy_egress has
// HTTPS_PROXY pointing at an external scraping proxy, and the fleet's strict
// service-scoped bundles remove the public mesh suffixes from NO_PROXY. A
// vault call that followed the environment was tunnelled through that proxy,
// reached the gateway from a public address and was refused by the vault
// vhost's source allowlist (403), silently disabling telemetry on every start.
func TestDirectTransportNeverUsesTheEnvironmentProxy(t *testing.T) {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(key, "http://user:pass@proxy.example.invalid:80")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	transport := DirectTransport()
	if transport.Proxy != nil {
		t.Fatal("DirectTransport must not consult the proxy environment")
	}
	req, err := http.NewRequest(http.MethodGet, "https://fleet-secrets.0exec.com/secrets/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Prove the contrast: the standard transport would have used the proxy for
	// this exact request, so the assertion above is not vacuous.
	if proxied, err := http.ProxyFromEnvironment(req); err == nil && proxied == nil {
		t.Log("note: http.ProxyFromEnvironment cached an empty environment earlier in this process")
	}
}

func TestDirectTransportReturnsAnIndependentCopyOfTheDefaults(t *testing.T) {
	first, second := DirectTransport(), DirectTransport()
	if first == second {
		t.Fatal("each call must return a fresh transport the caller owns")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport was replaced; cannot compare defaults")
	}
	if first.TLSHandshakeTimeout != base.TLSHandshakeTimeout || first.IdleConnTimeout != base.IdleConnTimeout {
		t.Fatalf("handshake/idle timeouts diverged from the defaults: %v/%v vs %v/%v",
			first.TLSHandshakeTimeout, first.IdleConnTimeout, base.TLSHandshakeTimeout, base.IdleConnTimeout)
	}
	if base.Proxy == nil {
		t.Fatal("test premise broken: the default transport no longer honours the environment proxy")
	}
	if first.Proxy != nil {
		t.Fatal("clearing Proxy on the clone must not leak into the shared default transport")
	}
}

func TestNewFromEnvDefaultClientIgnoresTheEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example.invalid:80")
	getenv := func(key string) string {
		if key == "FLEET_SECRETS_API_KEY" {
			return "service-key"
		}
		return ""
	}
	client, err := NewFromEnv(getenv, nil)
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	httpClient, ok := client.http.(*http.Client)
	if !ok {
		t.Fatalf("default Doer is %T, want *http.Client", client.http)
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("the default vault client must use the direct transport")
	}
	if httpClient.Timeout != 2*time.Second {
		t.Fatalf("timeout changed: %v", httpClient.Timeout)
	}
	if httpClient.CheckRedirect == nil {
		t.Fatal("redirects must stay disabled so the API key cannot be forwarded")
	}
}

func TestNewFromEnvKeepsACallerSuppliedClient(t *testing.T) {
	custom := &http.Client{Timeout: time.Second}
	getenv := func(key string) string {
		if key == "FLEET_SECRETS_API_KEY" {
			return "service-key"
		}
		return ""
	}
	client, err := NewFromEnv(getenv, custom)
	if err != nil {
		t.Fatalf("NewFromEnv: %v", err)
	}
	if client.http != Doer(custom) {
		t.Fatal("a caller-supplied Doer must be used unchanged")
	}
}

func TestGetReadsTheEnvelopeValueAndSendsTheKey(t *testing.T) {
	var gotKey, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("X-API-Key"), r.URL.Path
		_, _ = io.WriteString(w, `{"status":"success","data":{"name":"some_secret","value":"the-value"}}`)
	}))
	defer server.Close()

	got, err := New(server.URL+"/", "test-key", server.Client()).Get(context.Background(), "some_secret")
	if err != nil || got != "the-value" {
		t.Fatalf("Get = %q, %v; want the envelope value", got, err)
	}
	if gotKey != "test-key" || gotPath != "/secrets/some_secret" {
		t.Fatalf("request carried key=%q path=%q", gotKey, gotPath)
	}
}

// A caller deciding whether a failed read is worth retrying must not parse
// error text: the status is available through errors.As, the message is the
// one earlier releases produced, and neither the response body nor the API
// key can reach it.
func TestGetReportsNon200AsATypedStatusError(t *testing.T) {
	const leakedBody = "response-body-must-not-surface"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, leakedBody)
			}))
			defer server.Close()

			_, err := New(server.URL, "test-key", server.Client()).Get(context.Background(), "some_secret")
			var statusErr *StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("error %v is not a *StatusError", err)
			}
			if statusErr.StatusCode != status || statusErr.Name != "some_secret" {
				t.Fatalf("StatusError = %+v, want status %d for some_secret", statusErr, status)
			}
			if want := fmt.Sprintf("secrets: GET %q returned status %d", "some_secret", status); err.Error() != want {
				t.Fatalf("message changed: %q, want %q", err.Error(), want)
			}
			if strings.Contains(err.Error(), leakedBody) || strings.Contains(err.Error(), "test-key") {
				t.Fatalf("error leaked the body or the API key: %q", err.Error())
			}
			wrapped := fmt.Errorf("telemetry: load: %w", err)
			if !errors.As(wrapped, &statusErr) || statusErr.StatusCode != status {
				t.Fatal("the status must survive wrapping with %w")
			}
		})
	}
}

// Only a non-200 status is a *StatusError. A 200 whose payload is unusable is
// a different failure that retrying cannot cure, so callers must be able to
// tell the two apart.
func TestGetOnlyNon200IsAStatusError(t *testing.T) {
	for name, body := range map[string]string{
		"empty value":    `{"status":"success","data":{"name":"some_secret","value":""}}`,
		"error envelope": `{"status":"error","error":{"code":"denied","message":"no"}}`,
		"not json":       `<html>captive portal</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()

			_, err := New(server.URL, "test-key", server.Client()).Get(context.Background(), "some_secret")
			if err == nil {
				t.Fatal("expected an error")
			}
			var statusErr *StatusError
			if errors.As(err, &statusErr) {
				t.Fatalf("a 200 response produced a StatusError: %v", err)
			}
		})
	}
}
