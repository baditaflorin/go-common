// Package deploymentlease is the shared fail-closed client for the Fleet
// Deployment Authority's mTLS authorization and target-lease API.
package deploymentlease

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/deploymentauthorization"
	"github.com/baditaflorin/go-common/deploymentintent"
	"github.com/baditaflorin/go-common/internal/strictjson"
	"github.com/baditaflorin/go-common/spiffe"
)

const (
	MinLifetime              = 30 * time.Second
	MaxLifetime              = 10 * time.Minute
	maxResponse              = 256 << 10
	maxRequestTimeout        = 3 * time.Minute
	defaultRequestTimeout    = 160 * time.Second
	maxRegistryTokenLifetime = 65 * time.Minute
	maxRegistryTokenBytes    = 4096
)

var (
	ErrInvalidConfig = errors.New("deployment authority client configuration is invalid")
	ErrUnavailable   = errors.New("deployment authority is unavailable")
	ErrRejected      = errors.New("deployment authorization or lease was rejected")
	ErrInvalidLease  = errors.New("deployment authority returned an invalid lease")
)

// Lease is the durable authority's active reservation. Fence is monotonically
// increasing for a service/target pair and must be checked before each runtime
// mutation by the execution layer.
type Lease struct {
	LeaseID         string    `json:"lease_id"`
	DecisionID      string    `json:"decision_id"`
	AttemptID       string    `json:"attempt_id"`
	IntentID        string    `json:"intent_id"`
	IntentDigest    string    `json:"intent_digest"`
	ArtifactDigest  string    `json:"artifact_digest"`
	SourceCommit    string    `json:"source_commit"`
	RollbackDigest  string    `json:"rollback_digest"`
	Principal       string    `json:"principal"`
	ServiceID       string    `json:"service_id"`
	Environment     string    `json:"environment"`
	TargetPool      string    `json:"target_pool"`
	TargetID        string    `json:"target_id"`
	PolicyVersion   string    `json:"policy_version"`
	Fence           int64     `json:"fence"`
	State           string    `json:"state"`
	IssuedAt        time.Time `json:"issued_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	IntentExpiresAt time.Time `json:"intent_expires_at"`
}

// RegistryCredential is a single-repository, read-only pull token issued only
// for an active deployment lease. Token is held as bytes so callers can clear
// it as soon as the image pull finishes. String formatting always redacts it.
type RegistryCredential struct {
	CredentialID string    `json:"credential_id"`
	Token        []byte    `json:"-"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (c *RegistryCredential) UnmarshalJSON(data []byte) error {
	var wire struct {
		CredentialID string    `json:"credential_id"`
		Token        string    `json:"token"`
		ExpiresAt    time.Time `json:"expires_at"`
	}
	if err := strictjson.Decode(data, &wire); err != nil {
		return err
	}
	c.CredentialID = wire.CredentialID
	c.Token = []byte(wire.Token)
	c.ExpiresAt = wire.ExpiresAt
	return nil
}

func (c RegistryCredential) String() string {
	return fmt.Sprintf("RegistryCredential{CredentialID:%q ExpiresAt:%s Token:[REDACTED]}", c.CredentialID, c.ExpiresAt.UTC().Format(time.RFC3339))
}

func (c RegistryCredential) GoString() string { return c.String() }

// Clear zeroes the in-memory bearer token after use.
func (c *RegistryCredential) Clear() {
	if c == nil {
		return
	}
	clear(c.Token)
	c.Token = nil
}

// Config uses only private runtime configuration. Keys must be loaded from a
// protected file by the caller; public intent/evidence never supplies trust
// roots. ServerSPIFFEIDs is an exact peer allowlist.
type Config struct {
	Endpoint          string
	WorkloadAPISocket string
	ServerSPIFFEIDs   []string
	Principal         string
	SigningKeys       map[string]ed25519.PublicKey
	RequestTimeout    time.Duration
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Client struct {
	endpoint  string
	principal string
	keys      map[string]ed25519.PublicKey
	http      httpDoer
	close     func() error
}

// New creates a redirect-disabled mTLS client from the local SPIFFE Workload
// API. Proxy environment variables are ignored by go-common/spiffe.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if ctx == nil || validateConfig(cfg) != nil {
		return nil, ErrInvalidConfig
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = defaultRequestTimeout
	}
	if requestTimeout < time.Second || requestTimeout > maxRequestTimeout {
		return nil, ErrInvalidConfig
	}
	host, err := spiffe.NewHTTPClientWithTimeout(ctx, cfg.WorkloadAPISocket, cfg.ServerSPIFFEIDs, requestTimeout)
	if err != nil {
		return nil, ErrUnavailable
	}
	c, err := newWithHTTP(cfg, host, host.Close)
	if err != nil {
		_ = host.Close()
		return nil, err
	}
	return c, nil
}

// newWithHTTP is a test seam. It is unexported so callers cannot accidentally
// replace the production SPIFFE-authenticated transport.
func newWithHTTP(cfg Config, client httpDoer, closeFn func() error) (*Client, error) {
	if validateConfig(cfg) != nil || client == nil {
		return nil, ErrInvalidConfig
	}
	endpoint, err := normalizedEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	keys := make(map[string]ed25519.PublicKey, len(cfg.SigningKeys))
	for id, key := range cfg.SigningKeys {
		if strings.TrimSpace(id) != id || len(key) != ed25519.PublicKeySize {
			return nil, ErrInvalidConfig
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	return &Client{endpoint: endpoint, principal: cfg.Principal, keys: keys, http: client, close: closeFn}, nil
}

func validateConfig(cfg Config) error {
	if _, err := normalizedEndpoint(cfg.Endpoint); err != nil || strings.TrimSpace(cfg.Principal) != cfg.Principal || !deploymentintent.ValidActor(cfg.Principal) || len(cfg.SigningKeys) == 0 {
		return ErrInvalidConfig
	}
	for id, key := range cfg.SigningKeys {
		if strings.TrimSpace(id) != id || id == "" || len(key) != ed25519.PublicKeySize {
			return ErrInvalidConfig
		}
	}
	return nil
}

func normalizedEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrInvalidConfig
	}
	if strings.TrimSpace(raw) != raw {
		return "", ErrInvalidConfig
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (c *Client) Close() error {
	if c == nil || c.close == nil {
		return nil
	}
	return c.close()
}

// Acquire first asks the authority to evaluate the immutable intent, verifies
// the returned DSSE allow with protected keys, then reserves an exact logical
// target. Retries are safe only when callers reuse the same attemptID.
func (c *Client) Acquire(ctx context.Context, intent deploymentintent.V1, attemptID, targetID string) (Lease, error) {
	if c == nil || ctx == nil || !deploymentintent.ValidUUID(attemptID) || strings.TrimSpace(targetID) != targetID || targetID == "" {
		return Lease{}, ErrInvalidConfig
	}
	if err := deploymentintent.ValidateV1(intent, time.Now().UTC()); err != nil {
		return Lease{}, ErrRejected
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return Lease{}, ErrRejected
	}
	evidence, err := c.post(ctx, "/v1/authorizations", intentJSON, http.StatusCreated, "application/vnd.dsse.envelope.v1+json")
	if err != nil {
		return Lease{}, err
	}
	now := time.Now().UTC()
	verified, err := deploymentauthorization.VerifyV1(intent, c.principal, evidence, c.keys, now)
	if err != nil {
		return Lease{}, ErrRejected
	}
	requestBody, err := json.Marshal(struct {
		AttemptID     string          `json:"attempt_id"`
		TargetID      string          `json:"target_id"`
		Intent        json.RawMessage `json:"intent"`
		Authorization json.RawMessage `json:"authorization"`
	}{attemptID, targetID, intentJSON, evidence})
	if err != nil {
		return Lease{}, ErrRejected
	}
	response, err := c.post(ctx, "/v1/leases", requestBody, http.StatusOK, "application/json")
	if err != nil {
		return Lease{}, err
	}
	var lease Lease
	if err := decodeStrict(response, &lease); err != nil || !leaseMatchesIntent(lease, intent, attemptID, targetID, c.principal, verified.DecisionID, verified.PolicyVersion, time.Now().UTC()) {
		return Lease{}, ErrInvalidLease
	}
	return lease, nil
}

// Validate checks current ownership, fence, policy, and expiry at the
// authority. Call it immediately before each runtime mutation.
func (c *Client) Validate(ctx context.Context, lease Lease) (Lease, error) {
	return c.leaseAction(ctx, lease, "validate", struct {
		Fence int64 `json:"fence"`
	}{lease.Fence}, "active")
}

// Renew extends a current lease, up to the authority's ten-minute maximum.
func (c *Client) Renew(ctx context.Context, lease Lease, lifetime time.Duration) (Lease, error) {
	seconds := int64(lifetime / time.Second)
	if lifetime%time.Second != 0 || lifetime < MinLifetime || lifetime > MaxLifetime {
		return Lease{}, ErrInvalidConfig
	}
	return c.leaseAction(ctx, lease, "renew", struct {
		Fence           int64 `json:"fence"`
		LifetimeSeconds int64 `json:"lifetime_seconds"`
	}{lease.Fence, seconds}, "active")
}

// Finish releases the lease after a completed deployment or a verified
// rollback/failure outcome. Expiry remains the bounded recovery path if the
// runner is terminated before it can report completion.
func (c *Client) Finish(ctx context.Context, lease Lease, outcome string) error {
	if outcome != "completed" && outcome != "failed" {
		return ErrInvalidConfig
	}
	if c == nil || ctx == nil || !validLeaseIdentity(lease) {
		return ErrInvalidConfig
	}
	body, err := json.Marshal(struct {
		Fence   int64  `json:"fence"`
		Outcome string `json:"outcome"`
	}{lease.Fence, outcome})
	if err != nil {
		return ErrRejected
	}
	_, err = c.post(ctx, "/v1/leases/"+url.PathEscape(lease.LeaseID)+"/finish", body, http.StatusNoContent, "")
	return err
}

// IssueRegistryCredential asks the authority for a short-lived, repository-
// scoped read-only image-pull token. The authority refuses issuance without a
// currently active lease owned by this mTLS principal.
func (c *Client) IssueRegistryCredential(ctx context.Context, lease Lease) (RegistryCredential, error) {
	if c == nil || ctx == nil || !validLeaseIdentity(lease) || lease.State != "active" {
		return RegistryCredential{}, ErrInvalidConfig
	}
	body, err := json.Marshal(struct {
		Fence int64 `json:"fence"`
	}{lease.Fence})
	if err != nil {
		return RegistryCredential{}, ErrRejected
	}
	response, err := c.post(ctx, "/v1/leases/"+url.PathEscape(lease.LeaseID)+"/registry-credentials", body, http.StatusCreated, "application/json")
	if err != nil {
		return RegistryCredential{}, err
	}
	var credential RegistryCredential
	if err := decodeStrict(response, &credential); err != nil || !validRegistryCredential(credential, time.Now().UTC()) {
		credential.Clear()
		return RegistryCredential{}, ErrRejected
	}
	return credential, nil
}

// RevokeRegistryCredential invalidates the bearer token immediately after the
// image has been pulled. A bounded provider expiry remains the crash fallback.
func (c *Client) RevokeRegistryCredential(ctx context.Context, lease Lease, credentialID string) error {
	if c == nil || ctx == nil || !validLeaseIdentity(lease) || !deploymentintent.ValidUUID(credentialID) {
		return ErrInvalidConfig
	}
	path := "/v1/leases/" + url.PathEscape(lease.LeaseID) + "/registry-credentials/" + url.PathEscape(credentialID)
	_, err := c.request(ctx, http.MethodDelete, path, nil, http.StatusNoContent, "")
	return err
}

func validRegistryCredential(credential RegistryCredential, now time.Time) bool {
	return deploymentintent.ValidUUID(credential.CredentialID) && len(credential.Token) > 0 &&
		len(credential.Token) <= maxRegistryTokenBytes && !strings.ContainsAny(string(credential.Token), "\r\n") &&
		credential.ExpiresAt.After(now) && !credential.ExpiresAt.After(now.Add(maxRegistryTokenLifetime))
}

func (c *Client) leaseAction(ctx context.Context, lease Lease, action string, request any, expectedState string) (Lease, error) {
	if c == nil || ctx == nil || !validLeaseIdentity(lease) {
		return Lease{}, ErrInvalidConfig
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Lease{}, ErrRejected
	}
	response, err := c.post(ctx, "/v1/leases/"+url.PathEscape(lease.LeaseID)+"/"+action, body, http.StatusOK, "application/json")
	if err != nil {
		return Lease{}, err
	}
	var updated Lease
	if err := decodeStrict(response, &updated); err != nil || !sameLease(lease, updated, expectedState) {
		return Lease{}, ErrInvalidLease
	}
	return updated, nil
}

func (c *Client) post(ctx context.Context, path string, body []byte, expectedStatus int, expectedMediaType string) ([]byte, error) {
	return c.request(ctx, http.MethodPost, path, body, expectedStatus, expectedMediaType)
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, expectedStatus int, expectedMediaType string) ([]byte, error) {
	if c == nil || c.http == nil || ctx == nil || len(body) > deploymentauthorization.MaxEnvelopeBytes+deploymentintent.MaxBodyBytes+(64<<10) {
		return nil, ErrInvalidConfig
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInvalidConfig
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != expectedStatus || (expectedMediaType != "" && (mediaErr != nil || mediaType != expectedMediaType)) {
		return nil, ErrRejected
	}
	result, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(result) > maxResponse {
		return nil, ErrRejected
	}
	return result, nil
}

func decodeStrict(body []byte, dst any) error {
	return strictjson.Decode(body, dst)
}

func leaseMatchesIntent(l Lease, intent deploymentintent.V1, attemptID, targetID, principal, decisionID, policyVersion string, now time.Time) bool {
	digest, err := deploymentintent.DigestV1(intent)
	return err == nil && validLeaseIdentity(l) && l.DecisionID == decisionID && l.AttemptID == attemptID &&
		l.IntentID == intent.IntentID && l.IntentDigest == digest && l.ArtifactDigest == intent.ArtifactDigest &&
		l.SourceCommit == intent.SourceCommit && l.RollbackDigest == intent.RollbackDigest && l.Principal == principal &&
		l.ServiceID == intent.ServiceID && l.Environment == intent.Environment && l.TargetPool == intent.TargetPool &&
		l.TargetID == targetID && l.PolicyVersion == policyVersion && l.State == "active" &&
		!l.IssuedAt.After(now.Add(5*time.Second)) && l.ExpiresAt.After(now) &&
		!l.ExpiresAt.After(now.Add(MaxLifetime+5*time.Second)) && !l.ExpiresAt.After(l.IntentExpiresAt) &&
		l.IntentExpiresAt.Equal(intent.ExpiresAt)
}

func sameLease(old, updated Lease, state string) bool {
	return validLeaseIdentity(old) && validLeaseIdentity(updated) && updated.State == state &&
		old.LeaseID == updated.LeaseID && old.DecisionID == updated.DecisionID && old.AttemptID == updated.AttemptID &&
		old.IntentID == updated.IntentID && old.IntentDigest == updated.IntentDigest && old.ArtifactDigest == updated.ArtifactDigest &&
		old.SourceCommit == updated.SourceCommit && old.RollbackDigest == updated.RollbackDigest && old.Principal == updated.Principal &&
		old.ServiceID == updated.ServiceID && old.Environment == updated.Environment && old.TargetPool == updated.TargetPool &&
		old.TargetID == updated.TargetID && old.PolicyVersion == updated.PolicyVersion && old.Fence == updated.Fence &&
		old.IssuedAt.Equal(updated.IssuedAt) &&
		old.IntentExpiresAt.Equal(updated.IntentExpiresAt) && updated.ExpiresAt.After(time.Now().UTC()) &&
		!updated.ExpiresAt.After(updated.IntentExpiresAt)
}

func validLeaseIdentity(l Lease) bool {
	return deploymentintent.ValidUUID(l.LeaseID) && deploymentintent.ValidUUID(l.DecisionID) &&
		deploymentintent.ValidUUID(l.AttemptID) && deploymentintent.ValidUUID(l.IntentID) &&
		deploymentintent.ValidDigest(l.IntentDigest) && deploymentintent.ValidDigest(l.ArtifactDigest) &&
		deploymentintent.ValidDigest(l.RollbackDigest) && deploymentintent.ValidActor(l.Principal) &&
		deploymentintent.ValidServiceID(l.ServiceID) && l.Environment != "" && l.TargetPool != "" && l.TargetID != "" &&
		l.PolicyVersion != "" && l.Fence > 0 && !l.IssuedAt.IsZero() && !l.ExpiresAt.IsZero() && !l.IntentExpiresAt.IsZero() &&
		l.ExpiresAt.After(l.IssuedAt) && l.IntentExpiresAt.After(l.IssuedAt) && !l.ExpiresAt.After(l.IssuedAt.Add(MaxLifetime))
}
