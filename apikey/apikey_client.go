package apikey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/baditaflorin/go-common/header"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal, timeout-bounded HTTP client for the keystore.
// Construct one per process and reuse — don't churn HTTPClient instances.
type Client struct {
	BaseURL    string       // default from APIKEY_SERVICE_URL
	AdminToken string       // default from APIKEY_SERVICE_ADMIN_TOKEN
	HTTPClient *http.Client // default: 5s timeout, no redirects
	UserAgent  string       // default: "go-common/apikey"

	// AdminObs (optional) receives one AdminEvent per Issue / Revoke /
	// List / Purge call. promx.NewAdminCollectors returns an
	// implementation that records fleet-canonical Prometheus metrics.
	AdminObs AdminObserver
}

// Verify checks the key against the keystore. Returns ErrInvalidKey for
// definitive rejections (401), ErrKeystoreUnavailable for transient
// failures the caller may want to degrade through.
func (c *Client) Verify(ctx context.Context, key string) (*VerifyResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/verify", nil)
	if err != nil {
		return nil, fmt.Errorf("apikey verify: build req: %w", err)
	}
	req.Header.Set(header.VerifyKey, key)
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeystoreUnavailable, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return &VerifyResult{
			User:  resp.Header.Get(header.AuthUser),
			Scope: resp.Header.Get(header.AuthScope),
		}, nil
	case http.StatusUnauthorized:
		return nil, ErrInvalidKey
	default:
		return nil, fmt.Errorf("%w: HTTP %d", ErrKeystoreUnavailable, resp.StatusCode)
	}
}

// Issue mints a new key. Requires AdminToken.
func (c *Client) Issue(ctx context.Context, req IssueRequest) (*IssueResult, error) {
	if c.AdminToken == "" {
		return nil, ErrAdminTokenMissing
	}
	body, _ := json.Marshal(req)
	out := &IssueResult{}
	if err := c.adminCall(ctx, http.MethodPost, "/issue", body, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Revoke marks a key revoked. Returns whether anything was actually
// revoked (false if already revoked or unknown).
func (c *Client) Revoke(ctx context.Context, key string) (bool, error) {
	if c.AdminToken == "" {
		return false, ErrAdminTokenMissing
	}
	body, _ := json.Marshal(map[string]string{"key": key})
	var out struct {
		Revoked bool `json:"revoked"`
	}
	if err := c.adminCall(ctx, http.MethodPost, "/revoke", body, &out); err != nil {
		return false, err
	}
	return out.Revoked, nil
}

// List returns all known keys (capped at 500 by the server).
func (c *Client) List(ctx context.Context) ([]KeyMeta, error) {
	if c.AdminToken == "" {
		return nil, ErrAdminTokenMissing
	}
	var out struct {
		Keys []KeyMeta `json:"keys"`
	}
	if err := c.adminCall(ctx, http.MethodGet, "/list", nil, &out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}

// Purge drops expired/revoked rows. Returns how many were deleted.
func (c *Client) Purge(ctx context.Context) (int, error) {
	if c.AdminToken == "" {
		return 0, ErrAdminTokenMissing
	}
	var out struct {
		Purged int `json:"purged"`
	}
	if err := c.adminCall(ctx, http.MethodPost, "/purge", nil, &out); err != nil {
		return 0, err
	}
	return out.Purged, nil
}

func (c *Client) adminCall(ctx context.Context, method, path string, body []byte, out any) (retErr error) {
	start := time.Now()
	result := "ok"
	defer func() {
		if c.AdminObs != nil {
			c.AdminObs.ObserveAdmin(AdminEvent{
				Op:       adminOpFromPath(path),
				Result:   result,
				Duration: time.Since(start),
			})
		}
	}()

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		result = "client_error"
		return fmt.Errorf("apikey %s %s: %w", method, path, err)
	}
	req.Header.Set(header.AdminToken, c.AdminToken)
	req.Header.Set("User-Agent", c.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		result = "transport_error"
		return fmt.Errorf("%w: %v", ErrKeystoreUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		result = "unavailable"
		return fmt.Errorf("%w: HTTP %d", ErrKeystoreUnavailable, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		result = "unauthorized"
		return errors.New("apikey: admin unauthorized (check APIKEY_SERVICE_ADMIN_TOKEN)")
	}
	if resp.StatusCode >= 400 {
		result = "client_error"
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("apikey %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	// The keystore wraps every successful response in the fleet-canonical
	// response.Success envelope: {"status":"success","data":{...}}.
	// Decode the envelope first, then unmarshal the inner "data" payload
	// into the caller-supplied struct. Without this unwrap, json.Decode
	// sees {"status","data"} at the top level and silently produces
	// zero-value output — the root cause of the issue-returns-empty-key bug.
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("apikey %s %s: decode envelope: %w", method, path, err)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}
