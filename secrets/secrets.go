// Package secrets is the canonical client for go-fleet-secrets, the
// fleet vault. It centralizes the GET /secrets/<name> call plus the
// response-envelope decode so no consumer hand-rolls it.
//
// Why this package exists: go-fleet-dns-sync hand-rolled the vault read
// and decoded the secret's "value" at the JSON top level, while the
// vault wraps payloads in the go-common response envelope
// ({"status":"success","data":{"value":...}}). The mismatch read an
// empty value, fell through to an unset env fallback, and silently
// disabled the DNS reconciler for 10 days (2026-05). Defining the read
// + decode once here means that contract can't drift per-consumer.
//
// Transport note: the vault is reached over the public gateway FQDN
// (fleet-secrets.0exec.com) which, via split-horizon DNS, resolves to a
// private gateway IP from inside the docker mesh. That hop is legitimate
// but requires a safehttp client with SAFEHTTP_ALLOW_PRIVATE_IPS set (or
// a plain *http.Client for a docker-internal hostname). The caller owns
// that choice and passes the configured client in — this package does
// not build one.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/response"
)

const (
	defaultFleetSecretsURL = "https://fleet-secrets.0exec.com"
	maxAPIKeyFileBytes     = 8 << 10
)

var ErrMissingAPIKey = errors.New("secrets: Fleet Secrets API key is not configured")

// Doer is the minimal HTTP surface this client needs. Both *http.Client
// and the safehttp client satisfy it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client reads secrets from go-fleet-secrets.
type Client struct {
	baseURL string
	apiKey  string
	http    Doer
}

// New builds a Client. baseURL is the vault root (e.g.
// "https://fleet-secrets.0exec.com"); a trailing slash is trimmed.
// apiKey is the caller's fleet API key — the gateway translates it into
// the X-Auth-User principal the vault checks against each secret's
// consumers allowlist. httpClient is the caller's configured transport
// (see the transport note on the package doc).
func New(baseURL, apiKey string, httpClient Doer) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    httpClient,
	}
}

// NewFromEnv builds a client for the fleet's approved HTTPS endpoint and
// resolves its caller key from protected runtime configuration. A custom
// Doer is accepted for tests; nil uses a short-timeout HTTP client that never
// follows redirects, so the API key cannot be forwarded to another host.
func NewFromEnv(getenv func(string) string, httpClient Doer) (*Client, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	baseURL := strings.TrimSpace(getenv("FLEET_SECRETS_URL"))
	if baseURL == "" {
		baseURL = defaultFleetSecretsURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "fleet-secrets.0exec.com") || (parsed.Port() != "" && parsed.Port() != "443") || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("secrets: URL must use the approved HTTPS host without credentials or query data")
	}

	apiKey := strings.TrimSpace(getenv("FLEET_SECRETS_API_KEY"))
	if apiKey == "" {
		if path := strings.TrimSpace(getenv("FLEET_SECRETS_API_KEY_FILE")); path != "" {
			apiKey, err = readProtectedFile(path, maxAPIKeyFileBytes)
			if err != nil {
				return nil, errors.New("secrets: Fleet Secrets API key file is unavailable or has unsafe permissions")
			}
		}
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(getenv("FLEET_API_KEY"))
	}
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}

	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 2 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return New(baseURL, apiKey, httpClient), nil
}

// Get fetches a single secret's plaintext value by name, decoding the
// fleet response envelope's data.value. It never logs or embeds the
// secret value in any returned error.
//
// Errors:
//   - misconfiguration (nil client/transport, empty base URL)
//   - transport failure
//   - non-200 status (the status code is included; the body is not)
//   - envelope decode failure (via response.DecodeData; an error
//     envelope surfaces as *response.Error)
//   - a 200 success envelope whose value is empty
func (c *Client) Get(ctx context.Context, name string) (string, error) {
	if c == nil || c.http == nil {
		return "", fmt.Errorf("secrets: client not configured")
	}
	if c.baseURL == "" {
		return "", fmt.Errorf("secrets: base URL unset")
	}
	if !validSecretName(name) {
		return "", errors.New("secrets: name must be a simple identifier")
	}
	url := c.baseURL + "/secrets/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("secrets: build request for %q: %w", name, err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("secrets: request %q: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secrets: GET %q returned status %d", name, resp.StatusCode)
	}

	var data struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := response.DecodeData(resp.Body, &data); err != nil {
		return "", fmt.Errorf("secrets: decode %q: %w", name, err)
	}
	if data.Value == "" {
		return "", fmt.Errorf("secrets: %q present but value is empty", name)
	}
	return data.Value, nil
}

func validSecretName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}

func readProtectedFile(path string, limit int) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("not a protected regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > limit {
		return "", errors.New("file is unreadable, empty, or oversized")
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("file is empty")
	}
	return value, nil
}
