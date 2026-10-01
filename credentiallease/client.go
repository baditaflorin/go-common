package credentiallease

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	protocolPath       = "/v1/leases"
	defaultMaxTTL      = 5 * time.Minute
	absoluteMaxTTL     = 15 * time.Minute
	defaultRequestWait = 5 * time.Second
	defaultRevokeWait  = 3 * time.Second
	maxResponseBytes   = 16 << 10
)

var (
	ErrInvalidConfig       = errors.New("credentiallease: invalid client configuration")
	ErrInvalidRequest      = errors.New("credentiallease: invalid lease request")
	ErrIdentityUnavailable = errors.New("credentiallease: workload identity unavailable")
	ErrBrokerUnavailable   = errors.New("credentiallease: broker unavailable")
	ErrBrokerRejected      = errors.New("credentiallease: broker rejected request")
	ErrInvalidLease        = errors.New("credentiallease: broker returned an invalid lease")
	ErrLeaseClosed         = errors.New("credentiallease: lease is closed or expired")
	ErrInvalidAuth         = errors.New("credentiallease: invalid authentication placement")
	ErrTargetMismatch      = errors.New("credentiallease: request target does not match lease target origin")
	ErrRevokeFailed        = errors.New("credentiallease: lease revocation failed")
)

// IdentityTokenSource returns a short-lived workload identity proof for the
// broker audience. Implementations should obtain a task/workload-bound token
// from the runtime identity provider; they must not return a provider API key,
// broker admin token, or static shared secret.
type IdentityTokenSource interface {
	Token(context.Context, string) (string, error)
}

// IdentityTokenSourceFunc adapts a function to IdentityTokenSource.
type IdentityTokenSourceFunc func(context.Context, string) (string, error)

// Token implements IdentityTokenSource.
func (f IdentityTokenSourceFunc) Token(ctx context.Context, audience string) (string, error) {
	return f(ctx, audience)
}

// Event contains only non-secret lifecycle metadata. It deliberately omits
// task IDs, resource names, lease IDs, tokens, response bodies, and errors.
type Event struct {
	Operation string
	Result    string
	Status    int
	Duration  time.Duration
}

// Observer receives acquire, use, and revoke outcome metadata. Use events are
// emitted after target response headers arrive and contain only a low-cardinal
// result, status, and duration. Implementations must not add credentials,
// authenticated request headers, bodies, task IDs, or resource names to logs
// or traces.
type Observer interface {
	ObserveCredentialLease(Event)
}

// Config configures the broker client. Endpoint must be an HTTPS origin or
// path. TLSConfig may supply private roots and a client certificate for mTLS;
// insecure TLS settings are rejected. Identity is required and must provide a
// short-lived proof for BrokerAudience.
type Config struct {
	Endpoint       string
	BrokerAudience string
	Identity       IdentityTokenSource
	TLSConfig      *tls.Config
	MaxLeaseTTL    time.Duration
	RequestTimeout time.Duration
	RevokeTimeout  time.Duration
	Observer       Observer
}

// String and formatting methods redact identity providers and TLS material.
func (Config) String() string { return "credentiallease.Config([REDACTED])" }

func (c Config) GoString() string { return c.String() }

func (c Config) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, c.String()) }

func (c Config) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Config string `json:"config"`
	}{Config: "[REDACTED]"})
}

// Client talks to a task-bound credential broker. It is safe for concurrent
// use when its IdentityTokenSource and Observer are safe for concurrent use.
type Client struct {
	endpoint       string
	brokerAudience string
	identity       IdentityTokenSource
	http           *http.Client
	maxLeaseTTL    time.Duration
	requestTimeout time.Duration
	revokeTimeout  time.Duration
	observer       Observer
}

// NewClient validates configuration and creates an HTTPS-only client. Redirects
// are never followed because doing so could forward workload credentials to an
// unintended host. The transport does not use environment proxy settings.
func NewClient(cfg Config) (*Client, error) {
	endpoint, err := validateEndpoint(cfg.Endpoint)
	if err != nil || !nonBlankExact(cfg.BrokerAudience) || len(cfg.BrokerAudience) > 512 || cfg.Identity == nil {
		return nil, ErrInvalidConfig
	}

	maxTTL := cfg.MaxLeaseTTL
	if maxTTL == 0 {
		maxTTL = defaultMaxTTL
	}
	if maxTTL < time.Second || maxTTL > absoluteMaxTTL || maxTTL%time.Second != 0 {
		return nil, ErrInvalidConfig
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = defaultRequestWait
	}
	if requestTimeout < time.Second || requestTimeout > 30*time.Second {
		return nil, ErrInvalidConfig
	}
	revokeTimeout := cfg.RevokeTimeout
	if revokeTimeout == 0 {
		revokeTimeout = defaultRevokeWait
	}
	if revokeTimeout < time.Second || revokeTimeout > 10*time.Second {
		return nil, ErrInvalidConfig
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12} //nolint:gosec // TLS 1.2 is the minimum supported protocol.
	if cfg.TLSConfig != nil {
		tlsConfig = cfg.TLSConfig.Clone()
		if tlsConfig.InsecureSkipVerify || tlsConfig.MaxVersion != 0 && tlsConfig.MaxVersion < tls.VersionTLS12 {
			return nil, ErrInvalidConfig
		}
		if tlsConfig.MinVersion == 0 || tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
		if tlsConfig.MaxVersion != 0 && tlsConfig.MaxVersion < tlsConfig.MinVersion {
			return nil, ErrInvalidConfig
		}
	}

	transport := &http.Transport{
		Proxy:                  nil,
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  requestTimeout,
		IdleConnTimeout:        30 * time.Second,
		MaxIdleConns:           8,
		MaxIdleConnsPerHost:    4,
		MaxConnsPerHost:        8,
		MaxResponseHeaderBytes: 32 << 10,
		DisableCompression:     true,
	}
	return &Client{
		endpoint:       endpoint,
		brokerAudience: strings.TrimSpace(cfg.BrokerAudience),
		identity:       cfg.Identity,
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxLeaseTTL:    maxTTL,
		requestTimeout: requestTimeout,
		revokeTimeout:  revokeTimeout,
		observer:       cfg.Observer,
	}, nil
}

// CloseIdleConnections closes idle broker connections. Call it when a
// short-lived client is discarded; long-running services should reuse Client.
func (c *Client) CloseIdleConnections() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// WithLease obtains a credential, runs fn with a context bounded by the
// broker's expiry, closes access to the credential when fn returns, and
// synchronously requests revocation. If fn fails and revocation also fails,
// both errors are returned. Revocation uses a detached, bounded context so a
// canceled task still gets a cleanup attempt.
func (c *Client) WithLease(ctx context.Context, request Request, fn func(context.Context, *Lease) error) (retErr error) {
	if c == nil || ctx == nil || fn == nil {
		return ErrInvalidRequest
	}
	if c.http == nil || c.identity == nil || c.maxLeaseTTL < time.Second {
		return ErrInvalidConfig
	}
	request.Actions = append([]string(nil), request.Actions...)
	if err := request.validate(c.maxLeaseTTL); err != nil {
		return err
	}
	targetOrigin, err := canonicalHTTPSOrigin(request.TargetOrigin)
	if err != nil {
		return ErrInvalidRequest
	}
	request.TargetOrigin = targetOrigin

	identityCtx, identityCancel := context.WithTimeout(ctx, c.requestTimeout)
	token, err := c.identity.Token(identityCtx, c.brokerAudience)
	identityCancel()
	if err != nil || !validBearerToken(token) {
		token = ""
		c.observe("acquire", "identity_error", 0, 0)
		return ErrIdentityUnavailable
	}

	started := time.Now()
	leaseResponse, status, err := c.acquire(ctx, token, request)
	c.observe("acquire", resultFor(err), status, time.Since(started))
	if err != nil {
		token = ""
		return err
	}

	lease := &Lease{
		id:           leaseResponse.LeaseID,
		expiresAt:    leaseResponse.ExpiresAt,
		placement:    request.Auth,
		targetOrigin: targetOrigin,
		observer:     c.observer,
		state:        &leaseState{credential: []byte(leaseResponse.Credential), revokeProof: []byte(token)},
	}
	token = ""
	leaseResponse.Credential = ""
	leaseCtx, cancelLease := context.WithDeadline(ctx, lease.expiresAt)
	lease.ctx = leaseCtx

	defer func() {
		lease.state.closed.Store(true)
		cancelLease()
		lease.state.credentialMu.Lock()
		zero(lease.state.credential)
		lease.state.credential = nil
		fallbackProof := append([]byte(nil), lease.state.revokeProof...)
		zero(lease.state.revokeProof)
		lease.state.revokeProof = nil
		lease.state.credentialMu.Unlock()

		revokeCtx, cancelRevoke := context.WithTimeout(context.WithoutCancel(ctx), c.revokeTimeout)
		defer cancelRevoke()
		revokeStart := time.Now()
		revokeErr, revokeStatus := c.revokeAfterCallback(revokeCtx, lease.id, fallbackProof)
		zero(fallbackProof)
		c.observe("revoke", resultFor(revokeErr), revokeStatus, time.Since(revokeStart))
		if revokeErr != nil {
			if retErr != nil {
				retErr = errors.Join(retErr, ErrRevokeFailed)
			} else {
				retErr = ErrRevokeFailed
			}
		}
	}()
	return fn(leaseCtx, lease)
}

func (c *Client) acquire(ctx context.Context, identityToken string, request Request) (leaseResponse, int, error) {
	body, err := json.Marshal(acquireRequest{
		TaskID:       request.TaskID,
		Audience:     request.Audience,
		Resource:     request.Resource,
		TargetOrigin: request.TargetOrigin,
		Actions:      append([]string(nil), request.Actions...),
		TTLSeconds:   int64(request.TTL / time.Second),
		AuthMode:     request.Auth.mode,
		AuthHeader:   request.Auth.header,
	})
	if err != nil {
		return leaseResponse{}, 0, ErrInvalidRequest
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.endpoint+protocolPath, bytes.NewReader(body))
	if err != nil {
		return leaseResponse{}, 0, ErrBrokerUnavailable
	}
	idempotencyKey := make([]byte, 16)
	if _, err := rand.Read(idempotencyKey); err != nil {
		return leaseResponse{}, 0, ErrBrokerUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+identityToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotency-Key", hex.EncodeToString(idempotencyKey))
	zero(idempotencyKey)
	resp, err := c.http.Do(req)
	req.Header.Del("Authorization")
	if err != nil {
		return leaseResponse{}, 0, ErrBrokerUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		if resp.StatusCode >= 500 || resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return leaseResponse{}, resp.StatusCode, ErrBrokerUnavailable
		}
		if resp.StatusCode >= 400 {
			return leaseResponse{}, resp.StatusCode, ErrBrokerRejected
		}
		return leaseResponse{}, resp.StatusCode, ErrInvalidLease
	}
	var out leaseResponse
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		zero(raw)
		return leaseResponse{}, resp.StatusCode, ErrInvalidLease
	}
	defer zero(raw)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		c.cleanupMalformedResponse(ctx, identityToken, out.LeaseID)
		out.Credential = ""
		return leaseResponse{}, resp.StatusCode, ErrInvalidLease
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		c.cleanupMalformedResponse(ctx, identityToken, out.LeaseID)
		out.Credential = ""
		return leaseResponse{}, resp.StatusCode, ErrInvalidLease
	}
	if err := out.validate(request, time.Now()); err != nil {
		c.cleanupMalformedResponse(ctx, identityToken, out.LeaseID)
		out.Credential = ""
		return leaseResponse{}, resp.StatusCode, ErrInvalidLease
	}
	return out, resp.StatusCode, nil
}

func (c *Client) cleanupMalformedResponse(ctx context.Context, identityToken, leaseID string) {
	if !validLeaseID(leaseID) {
		return
	}
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.revokeTimeout)
	defer cancel()
	started := time.Now()
	err, status := c.revokeWithToken(revokeCtx, identityToken, leaseID)
	c.observe("revoke", resultFor(err), status, time.Since(started))
}

func (c *Client) revokeAfterCallback(ctx context.Context, leaseID string, fallbackProof []byte) (error, int) {
	if len(fallbackProof) > 0 {
		err, status := c.revokeWithToken(ctx, string(fallbackProof), leaseID)
		if err == nil || status != http.StatusUnauthorized && status != http.StatusForbidden {
			return err, status
		}
	}
	identityCtx, cancel := context.WithTimeout(ctx, c.revokeTimeout)
	token, err := c.identity.Token(identityCtx, c.brokerAudience)
	cancel()
	if err != nil || !validBearerToken(token) {
		token = ""
		return ErrRevokeFailed, 0
	}
	revokeErr, status := c.revokeWithToken(ctx, token, leaseID)
	token = ""
	return revokeErr, status
}

func (c *Client) revokeWithToken(ctx context.Context, identityToken, leaseID string) (error, int) {
	requestCtx, cancel := context.WithTimeout(ctx, c.revokeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodDelete, c.endpoint+protocolPath+"/"+url.PathEscape(leaseID), nil)
	if err != nil {
		return ErrRevokeFailed, 0
	}
	req.Header.Set("Authorization", "Bearer "+identityToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	req.Header.Del("Authorization")
	if err != nil {
		return ErrRevokeFailed, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return ErrRevokeFailed, resp.StatusCode
	}
	return nil, resp.StatusCode
}

func (c *Client) observe(operation, result string, status int, duration time.Duration) {
	observeEvent(c.observer, operation, result, status, duration)
}

func observeEvent(observer Observer, operation, result string, status int, duration time.Duration) {
	if observer != nil {
		defer func() { _ = recover() }()
		observer.ObserveCredentialLease(Event{Operation: operation, Result: result, Status: status, Duration: duration})
	}
}

func resultFor(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrBrokerRejected):
		return "rejected"
	case errors.Is(err, ErrInvalidLease):
		return "invalid_response"
	default:
		return "error"
	}
}

func validateEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 2048 {
		return "", ErrInvalidConfig
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", ErrInvalidConfig
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func validBearerToken(token string) bool {
	if token == "" || len(token) > 8192 || strings.TrimSpace(token) != token {
		return false
	}
	for _, r := range token {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~+/=", r)) {
			return false
		}
	}
	return true
}

func (r Request) validate(maxTTL time.Duration) error {
	if !nonBlankExact(r.TaskID) || !nonBlankExact(r.Audience) || !nonBlankExact(r.Resource) || r.Resource == "*" || r.TTL < time.Second || r.TTL%time.Second != 0 || r.TTL > maxTTL {
		return ErrInvalidRequest
	}
	if len(r.TargetOrigin) > 2048 {
		return ErrInvalidRequest
	}
	if _, err := canonicalHTTPSOrigin(r.TargetOrigin); err != nil {
		return ErrInvalidRequest
	}
	if len(r.TaskID) > 256 || len(r.Audience) > 512 || len(r.Resource) > 2048 || len(r.Actions) == 0 || len(r.Actions) > 32 {
		return ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(r.Actions))
	for _, action := range r.Actions {
		if !nonBlankExact(action) || action == "*" || len(action) > 128 {
			return ErrInvalidRequest
		}
		if _, ok := seen[action]; ok {
			return ErrInvalidRequest
		}
		seen[action] = struct{}{}
	}
	if err := r.Auth.validate(); err != nil {
		return err
	}
	return nil
}

func nonBlankExact(s string) bool { return s != "" && strings.TrimSpace(s) == s }

func (Client) String() string { return "credentiallease.Client([REDACTED])" }

func (c Client) GoString() string { return c.String() }

func (c Client) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, c.String()) }

func (c Client) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Client string `json:"client"`
	}{Client: "[REDACTED]"})
}
