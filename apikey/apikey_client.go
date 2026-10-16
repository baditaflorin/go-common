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
	"net/url"
	"strings"
	"time"
)

// Client is a minimal, timeout-bounded HTTP client for the keystore.
// Construct one per process and reuse — don't churn HTTPClient instances.
type Client struct {
	BaseURL string // default from APIKEY_SERVICE_URL (the first of APIKEY_SERVICE_URLS when that is set)
	// Endpoints (optional) is the ordered list of keystore nodes, own host
	// first. Empty means BaseURL alone. See endpoints.go.
	Endpoints []string
	// HedgeDelay (optional) is how long Verify waits for one endpoint before
	// also asking the next; default DefaultHedgeDelay.
	HedgeDelay time.Duration
	// VerifyObs (optional) receives one VerifyEvent per Verify call.
	VerifyObs  VerifyObserver
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
// failures the caller may want to degrade through. With several Endpoints
// it asks the nearest healthy one and hedges to the next (endpoints.go).
func (c *Client) Verify(ctx context.Context, key string) (*VerifyResult, error) {
	return c.verifyAcross(ctx, key)
}

// verifyOnce asks one endpoint and records the outcome on its breaker.
func (c *Client) verifyOnce(ctx context.Context, base, key string, hedge bool) verifyOutcome {
	out := verifyOutcome{endpoint: base}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/verify", nil)
	if err != nil {
		out.err = fmt.Errorf("apikey verify: build req: %w", err)
		return out
	}
	req.Header.Set(header.VerifyKey, key)
	req.Header.Set("User-Agent", c.UserAgent)
	if hedge {
		req.Header.Set(HedgeHeader, "1")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		// A request we cancelled ourselves (another endpoint answered first)
		// says nothing about this endpoint's health.
		if ctx.Err() == nil {
			breakerFor(base).record(false, time.Now())
		}
		out.err = fmt.Errorf("%w: %v", ErrKeystoreUnavailable, err)
		return out
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	breakerFor(base).record(resp.StatusCode < 500, time.Now())
	switch resp.StatusCode {
	case http.StatusOK:
		out.result = &VerifyResult{
			User:  resp.Header.Get(header.AuthUser),
			Scope: resp.Header.Get(header.AuthScope),
			Tier:  resp.Header.Get(header.AuthTier),
		}
	case http.StatusUnauthorized:
		out.err = ErrInvalidKey
	default:
		out.err = fmt.Errorf("%w: HTTP %d", ErrKeystoreUnavailable, resp.StatusCode)
	}
	return out
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

// RevokeID revokes the key with the given id (KeyMeta.KeyID or KeyID(value)).
// It needs a keystore that supports ids (go_apikey_service >= 0.4.9).
func (c *Client) RevokeID(ctx context.Context, keyID string) (bool, error) {
	if c.AdminToken == "" {
		return false, ErrAdminTokenMissing
	}
	keyID = strings.TrimSpace(keyID)
	if !strings.HasPrefix(keyID, "kid_") {
		return false, errors.New("apikey: RevokeID needs a key id (kid_...)")
	}
	body, _ := json.Marshal(map[string]string{"key_id": keyID})
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

// ListExact returns only the complete identity intersection of user, scope,
// and tier. Every selector is required: partial filters are intentionally not
// supported because they would make this admin endpoint a broad key-discovery
// mechanism. Callers must treat Truncated as a fail-closed condition.
func (c *Client) ListExact(ctx context.Context, user, scope, tier string) (ExactListResult, error) {
	user, scope, tier = strings.TrimSpace(user), strings.TrimSpace(scope), strings.TrimSpace(tier)
	if user == "" || scope == "" || tier == "" {
		return ExactListResult{}, errors.New("apikey: ListExact requires non-empty user, scope, and tier")
	}
	if c.AdminToken == "" {
		return ExactListResult{}, ErrAdminTokenMissing
	}
	query := url.Values{}
	query.Set("user", user)
	query.Set("scope", scope)
	query.Set("tier", tier)
	var out ExactListResult
	if err := c.adminCall(ctx, http.MethodGet, "/list?"+query.Encode(), nil, &out); err != nil {
		return ExactListResult{}, err
	}
	return out, nil
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

	// Admin calls go to the first endpoint that accepts a connection. Only an
	// idempotent call moves on after any transport error; a non-idempotent one
	// (issue) moves on only when the request never reached the server (dial
	// error), so a slow node can never make it mint two keys. An HTTP answer
	// of any status is final.
	idempotent := adminOpFromPath(path) != "issue"
	var resp *http.Response
	for i, base := range c.endpointList() {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
		if err != nil {
			result = "client_error"
			return fmt.Errorf("apikey %s %s: %w", method, path, err)
		}
		req.Header.Set(header.AdminToken, c.AdminToken)
		req.Header.Set("User-Agent", c.UserAgent)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err = c.HTTPClient.Do(req)
		if err == nil {
			break
		}
		last := i == len(c.endpointList())-1
		if last || ctx.Err() != nil || (!idempotent && !isDialError(err)) {
			result = "transport_error"
			return fmt.Errorf("%w: %v", ErrKeystoreUnavailable, err)
		}
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
