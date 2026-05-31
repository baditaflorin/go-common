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
	"fmt"
	"net/http"
	"strings"

	"github.com/baditaflorin/go-common/response"
)

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
