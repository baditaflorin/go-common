package credentiallease

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AuthMode selects the one supported way to place a leased credential on an
// outbound request.
type AuthMode string

const (
	AuthBearer       AuthMode = "bearer"
	AuthAPIKeyHeader AuthMode = "api_key_header"
)

// AuthPlacement binds a lease to one HTTP authentication header. Use
// BearerAuth or APIKeyHeader to construct it; its fields stay private so the
// lease cannot be redirected into query parameters or arbitrary request data.
type AuthPlacement struct {
	mode   AuthMode
	header string
}

// BearerAuth configures Authorization: Bearer <credential>.
func BearerAuth() AuthPlacement { return AuthPlacement{mode: AuthBearer} }

// APIKeyHeader configures direct placement in a target-specific API key
// header, such as X-API-Key. Authorization, cookie, hop-by-hop, and framing
// headers are rejected.
func APIKeyHeader(name string) AuthPlacement {
	return AuthPlacement{mode: AuthAPIKeyHeader, header: http.CanonicalHeaderKey(strings.TrimSpace(name))}
}

func (a AuthPlacement) validate() error {
	switch a.mode {
	case AuthBearer:
		if a.header != "" {
			return ErrInvalidAuth
		}
	case AuthAPIKeyHeader:
		if !validHeaderName(a.header) {
			return ErrInvalidAuth
		}
		lower := strings.ToLower(a.header)
		switch lower {
		case "authorization", "proxy-authorization", "proxy-authenticate", "www-authenticate", "cookie", "set-cookie", "host", "content-length", "transfer-encoding", "connection", "keep-alive", "proxy-connection", "te", "trailer", "upgrade", "accept", "accept-encoding", "content-type", "user-agent", "referer", "origin", "forwarded":
			return ErrInvalidAuth
		}
		if strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "sec-") {
			return ErrInvalidAuth
		}
	default:
		return ErrInvalidAuth
	}
	return nil
}

// Request asks for one task-bound lease. The broker must compare TaskID with
// the authenticated workload identity and enforce Audience, Resource, Actions,
// TTL, and Auth together as one policy decision.
type Request struct {
	TaskID       string
	Audience     string
	Resource     string
	TargetOrigin string
	Actions      []string
	TTL          time.Duration
	Auth         AuthPlacement
}

// Lease gives callback-scoped access to one broker-issued credential. The
// credential itself is private and is zeroed on callback return on a best-
// effort basis. A copied Lease is closed at the same time.
type Lease struct {
	id           string
	expiresAt    time.Time
	placement    AuthPlacement
	targetOrigin string
	ctx          context.Context
	state        *leaseState
	observer     Observer
}

type leaseState struct {
	closed       atomic.Bool
	credentialMu sync.RWMutex
	credential   []byte
	revokeProof  []byte
}

// ID returns the non-secret broker lease identifier. Do not use it as an
// authorization credential.
func (l *Lease) ID() string {
	if l == nil {
		return ""
	}
	return l.id
}

// ExpiresAt returns the broker-reported expiration time.
func (l *Lease) ExpiresAt() time.Time {
	if l == nil {
		return time.Time{}
	}
	return l.expiresAt
}

// Do sends one authenticated request to the target origin fixed by the lease
// request. It clones req, applies the credential privately, bounds the request
// by both the lease context and req.Context(), and disables redirects so a
// custom credential header cannot be forwarded elsewhere.
func (l *Lease) Do(client *http.Client, req *http.Request) (*http.Response, error) {
	if l == nil || client == nil || req == nil || req.URL == nil || l.state == nil {
		return nil, ErrLeaseClosed
	}
	if l.state.closed.Load() || l.ctx == nil || l.ctx.Err() != nil || !time.Now().Before(l.expiresAt) {
		return nil, ErrLeaseClosed
	}
	origin, err := originForURL(req.URL)
	if err != nil || origin != l.targetOrigin {
		return nil, ErrTargetMismatch
	}
	if err := l.placement.validate(); err != nil {
		return nil, err
	}
	name := "Authorization"
	valuePrefix := "Bearer "
	if l.placement.mode == AuthAPIKeyHeader {
		name = l.placement.header
		valuePrefix = ""
	}
	if req.Header.Get(name) != "" {
		return nil, ErrInvalidAuth
	}

	l.state.credentialMu.RLock()
	defer l.state.credentialMu.RUnlock()
	if l.state.closed.Load() || len(l.state.credential) == 0 || !validHeaderValue(l.state.credential) {
		return nil, ErrLeaseClosed
	}
	callCtx, cancel := context.WithCancel(l.ctx)
	stop := context.AfterFunc(req.Context(), cancel)
	defer func() {
		stop()
		cancel()
	}()
	outbound := req.Clone(callCtx)
	if outbound.Header == nil {
		outbound.Header = make(http.Header)
	}
	outbound.Header.Set(name, valuePrefix+string(l.state.credential))
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	started := time.Now()
	resp, err := clientCopy.Do(outbound)
	outbound.Header.Del(name)
	result, status := "http_error", 0
	if err != nil {
		result = "transport_error"
	} else {
		status = resp.StatusCode
		if status >= http.StatusOK && status < http.StatusMultipleChoices {
			result = "ok"
		}
	}
	observeEvent(l.observer, "use", result, status, time.Since(started))
	return resp, err
}

// String, GoString, Format, and MarshalJSON redact all lease internals so
// formatting or serializing a Lease cannot reveal its credential.
func (Lease) String() string { return "credentiallease.Lease([REDACTED])" }

// GoString implements fmt.GoStringer.
func (l Lease) GoString() string { return l.String() }

// Format implements fmt.Formatter and never formats lease fields.
func (l Lease) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, l.String()) }

// MarshalJSON emits a redaction marker, never lease metadata or credential.
func (l Lease) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Lease string `json:"lease"`
	}{Lease: "[REDACTED]"})
}

type acquireRequest struct {
	TaskID       string   `json:"task_id"`
	Audience     string   `json:"audience"`
	Resource     string   `json:"resource"`
	TargetOrigin string   `json:"target_origin"`
	Actions      []string `json:"actions"`
	TTLSeconds   int64    `json:"ttl_seconds"`
	AuthMode     AuthMode `json:"auth_mode"`
	AuthHeader   string   `json:"auth_header,omitempty"`
}

type leaseResponse struct {
	LeaseID    string    `json:"lease_id"`
	Credential string    `json:"credential"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (leaseResponse) String() string { return "credentiallease.leaseResponse([REDACTED])" }

func (r leaseResponse) GoString() string { return r.String() }

func (r leaseResponse) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, r.String()) }

func (r leaseResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Response string `json:"response"`
	}{Response: "[REDACTED]"})
}

func (r leaseResponse) validate(request Request, now time.Time) error {
	if !validLeaseID(r.LeaseID) || len(r.Credential) == 0 || len(r.Credential) > 8192 || !validHeaderValue([]byte(r.Credential)) {
		return ErrInvalidLease
	}
	if r.ExpiresAt.IsZero() || !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(request.TTL)) {
		return ErrInvalidLease
	}
	if request.Auth.mode == AuthBearer && !validBearerToken(r.Credential) {
		return ErrInvalidLease
	}
	return nil
}

func validLeaseID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validHeaderName(name string) bool {
	if name == "" || http.CanonicalHeaderKey(name) != name {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func validHeaderValue(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, b := range value {
		if b == '\r' || b == '\n' || b == 0x7f || b < 0x20 {
			return false
		}
	}
	return true
}

func canonicalHTTPSOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path != "" && u.Path != "/" {
		return "", ErrInvalidRequest
	}
	return canonicalOrigin(u)
}

func originForURL(u *url.URL) (string, error) {
	if u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return "", ErrTargetMismatch
	}
	return canonicalOrigin(u)
}

func canonicalOrigin(u *url.URL) (string, error) {
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return "", ErrInvalidRequest
	}
	port := u.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", ErrInvalidRequest
		}
		if portNumber == 443 {
			port = ""
		}
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	return "https://" + host, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
