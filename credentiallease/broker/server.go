// Package broker implements the server side of the credentiallease v1 HTTP
// contract. Trust-domain identity, policy, durable storage, audit, and provider
// adapters are injected; the package contains no production credentials or
// permissive defaults.
package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/internal/strictjson"
)

const (
	defaultMaxTTL          = 5 * time.Minute
	absoluteMaxTTL         = 15 * time.Minute
	maxBodyBytes           = 8 << 10
	maxCredentialBytes     = 4 << 10
	cleanupTimeout         = 3 * time.Second
	issueTimeout           = 10 * time.Second
	providerRevokeTimeout  = 5 * time.Second
	idempotencyRetryWindow = 24 * time.Hour
)

var (
	ErrInvalidConfig       = errors.New("credentiallease broker: invalid configuration")
	ErrInvalidProof        = errors.New("credentiallease broker: invalid workload proof")
	ErrDenied              = errors.New("credentiallease broker: request denied")
	ErrIdempotencyConflict = errors.New("credentiallease broker: idempotency key reused with different request")
	ErrIdempotencyUsed     = errors.New("credentiallease broker: idempotency key already used")
	ErrLeaseNotFound       = errors.New("credentiallease broker: lease not found")
)

// Identity is returned only by a configured verifier after it checks proof
// signature, issuer, audience, expiry, and required workload claims. TaskID
// must come from the verified proof, not from the HTTP request.
type Identity struct {
	Issuer     string
	Subject    string
	WorkloadID string
	TaskID     string
}

// ProofVerifier verifies a short-lived workload proof for the broker's exact
// audience. Implementations must reject unsigned, expired, wrong-issuer, and
// wrong-audience proofs and bind TaskID to a signed claim.
type ProofVerifier interface {
	Verify(context.Context, string, string) (Identity, error)
}

// LeaseRequest is the requested resource scope. A policy must authorize every
// field; callers cannot supply an issuer, role, credential, or provider secret.
type LeaseRequest struct {
	TaskID       string        `json:"task_id"`
	Audience     string        `json:"audience"`
	Resource     string        `json:"resource"`
	TargetOrigin string        `json:"target_origin"`
	Actions      []string      `json:"actions"`
	TTL          time.Duration `json:"-"`
	AuthMode     string        `json:"auth_mode"`
	AuthHeader   string        `json:"auth_header,omitempty"`
}

// PrincipalKey provides an opaque, stable namespace for lease ownership and
// idempotency. It is a hash of the verified issuer, subject, workload, and
// task identifiers; it is not an authorization credential.
func (i Identity) PrincipalKey() string {
	h := sha256.Sum256([]byte(i.Issuer + "\x00" + i.Subject + "\x00" + i.WorkloadID + "\x00" + i.TaskID))
	return hex.EncodeToString(h[:])
}

// Grant is the exact policy-approved scope passed to a provider adapter.
// MaxTTL can reduce the requested lifetime; it can never extend it.
type Grant struct {
	Provider string
	Request  LeaseRequest
	MaxTTL   time.Duration
}

// Policy authorizes one exact request against the verified workload identity.
type Policy interface {
	Authorize(context.Context, Identity, LeaseRequest) (Grant, error)
}

// IssuedCredential holds a provider-issued value and opaque provider revocation
// handle. Returning transfers ownership of both byte slices to the server,
// which zeroes them after use on a best-effort basis. Provider adapters must
// enforce the approved Grant scope and expiry and make Revoke idempotent.
type IssuedCredential struct {
	Value        []byte
	ExpiresAt    time.Time
	RevokeHandle []byte
}

func (IssuedCredential) String() string               { return "broker.IssuedCredential([REDACTED])" }
func (IssuedCredential) GoString() string             { return "broker.IssuedCredential([REDACTED])" }
func (IssuedCredential) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// Issuer creates and revokes one provider credential for an approved grant.
type Issuer interface {
	Issue(context.Context, Grant) (IssuedCredential, error)
	Revoke(context.Context, []byte) error
}

// LeaseReservation must be inserted atomically with its idempotency key. A
// production store must retain reservations through the full lease lifetime
// plus its retry window, so a lost HTTP response cannot mint a second lease.
type LeaseReservation struct {
	IdempotencyDigest string
	RequestDigest     string
	Principal         string
	LeaseID           string
	CreatedAt         time.Time
	RetainUntil       time.Time
}

// LeaseRecord contains only lease metadata and a provider revocation handle;
// it never contains the issued credential. Production stores must encrypt the
// revoke handle at rest and implement methods transactionally.
type LeaseRecord struct {
	LeaseID      string
	Principal    string
	Provider     string
	RevokeHandle []byte
	ExpiresAt    time.Time
	RevokedAt    time.Time
}

func (LeaseRecord) String() string               { return "broker.LeaseRecord([REDACTED])" }
func (LeaseRecord) GoString() string             { return "broker.LeaseRecord([REDACTED])" }
func (LeaseRecord) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// Store supplies the atomic durable lease ledger. Reserve must return
// ErrIdempotencyUsed for an identical previously reserved key and
// ErrIdempotencyConflict for the same key with different request/principal.
// Get must return ErrLeaseNotFound for an unknown lease. MarkRevoked is
// idempotent. Implementations must copy byte slices before returning.
type Store interface {
	Reserve(context.Context, LeaseReservation) error
	Commit(context.Context, LeaseRecord) error
	Get(context.Context, string) (LeaseRecord, error)
	MarkRevoked(context.Context, string, time.Time) error
}

// AuditEvent contains non-secret lifecycle metadata only. Implementations
// must not attach credentials, identity proofs, request headers, provider
// response bodies, or raw error text.
type AuditEvent struct {
	Operation string
	Result    string
	Identity  string
	TaskID    string
	Audience  string
	Resource  string
	Actions   []string
	Provider  string
	LeaseID   string
	ExpiresAt time.Time
	At        time.Time
}

// Auditor records protected lifecycle metadata. Audit failure blocks issuance
// and revocation attempts.
type Auditor interface {
	Record(context.Context, AuditEvent) error
}

// Config wires the trust-domain-specific components. All dependencies are
// mandatory, including an audit sink; zero-value or incomplete servers fail
// closed at construction.
type Config struct {
	Audience string
	MaxTTL   time.Duration
	Verifier ProofVerifier
	Policy   Policy
	Store    Store
	Auditor  Auditor
	Issuers  map[string]Issuer
	Now      func() time.Time
}

// Server handles POST /v1/leases and DELETE /v1/leases/{id}.
type Server struct {
	audience string
	maxTTL   time.Duration
	verifier ProofVerifier
	policy   Policy
	store    Store
	auditor  Auditor
	issuers  map[string]Issuer
	now      func() time.Time
}

// New validates all mandatory dependencies and returns a broker HTTP handler.
// A non-nil empty issuer map is valid for deny-all policies; grants without a
// configured issuer are rejected by acquire before provider access.
func New(cfg Config) (*Server, error) {
	if !validClaim(cfg.Audience, 512) || cfg.Verifier == nil || cfg.Policy == nil || cfg.Store == nil || cfg.Auditor == nil || cfg.Issuers == nil {
		return nil, ErrInvalidConfig
	}
	maxTTL := cfg.MaxTTL
	if maxTTL == 0 {
		maxTTL = defaultMaxTTL
	}
	if maxTTL < time.Second || maxTTL > absoluteMaxTTL || maxTTL%time.Second != 0 {
		return nil, ErrInvalidConfig
	}
	issuers := make(map[string]Issuer, len(cfg.Issuers))
	for name, issuer := range cfg.Issuers {
		if !validClaim(name, 128) || issuer == nil {
			return nil, ErrInvalidConfig
		}
		issuers[name] = issuer
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Server{audience: cfg.Audience, maxTTL: maxTTL, verifier: cfg.Verifier, policy: cfg.Policy, store: cfg.Store, auditor: cfg.Auditor, issuers: issuers, now: now}, nil
}

// ServeHTTP dispatches only the v1 lease endpoints.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {
			writeError(w, http.StatusInternalServerError, "internal_error")
		}
	}()
	if s == nil || s.verifier == nil || s.policy == nil || s.store == nil || s.auditor == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
		s.acquire(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/leases/"):
		s.revoke(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

type wireRequest struct {
	TaskID       string   `json:"task_id"`
	Audience     string   `json:"audience"`
	Resource     string   `json:"resource"`
	TargetOrigin string   `json:"target_origin"`
	Actions      []string `json:"actions"`
	TTLSeconds   int64    `json:"ttl_seconds"`
	AuthMode     string   `json:"auth_mode"`
	AuthHeader   string   `json:"auth_header,omitempty"`
}

func (s *Server) acquire(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	r.Header.Del("Idempotency-Key")
	if len(keys) != 1 || !validIdempotencyKey(keys[0]) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	key := keys[0]
	requested, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	if requested.TaskID != identity.TaskID {
		s.audit(r.Context(), AuditEvent{Operation: "acquire", Result: "denied", Identity: identity.PrincipalKey(), TaskID: identity.TaskID, Audience: requested.Audience, Resource: requested.Resource, Actions: requested.Actions, At: s.now().UTC()})
		writeError(w, http.StatusForbidden, "denied")
		return
	}
	if err := validateRequest(requested, s.maxTTL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	grant, err := s.policy.Authorize(r.Context(), identity, requested)
	if err != nil || !validGrant(grant, requested, s.maxTTL) {
		s.audit(r.Context(), auditFor("acquire", "denied", identity, requested, grant.Provider, "", time.Time{}, s.now()))
		writeError(w, http.StatusForbidden, "denied")
		return
	}
	if _, ok := s.issuers[grant.Provider]; !ok {
		s.audit(r.Context(), auditFor("acquire", "denied", identity, requested, grant.Provider, "", time.Time{}, s.now()))
		writeError(w, http.StatusForbidden, "denied")
		return
	}

	grant.Request.Actions = append([]string(nil), grant.Request.Actions...)
	grant.Request.TTL = min(grant.Request.TTL, grant.MaxTTL)
	now := s.now().UTC()
	leaseID, err := randomID()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	keyDigest := sha256.Sum256([]byte(identity.PrincipalKey() + "\x00" + strings.ToLower(key)))
	requestDigest, err := requestDigest(identity, requested, grant.Provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reservation := LeaseReservation{IdempotencyDigest: hex.EncodeToString(keyDigest[:]), RequestDigest: requestDigest, Principal: identity.PrincipalKey(), LeaseID: leaseID, CreatedAt: now, RetainUntil: now.Add(grant.Request.TTL + idempotencyRetryWindow)}
	if err := s.store.Reserve(r.Context(), reservation); err != nil {
		switch {
		case errors.Is(err, ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, "idempotency_conflict")
		case errors.Is(err, ErrIdempotencyUsed):
			writeError(w, http.StatusConflict, "idempotency_used")
		default:
			writeError(w, http.StatusServiceUnavailable, "unavailable")
		}
		return
	}

	baseEvent := auditFor("acquire", "issue_started", identity, requested, grant.Provider, leaseID, now.Add(grant.Request.TTL), now)
	if err := s.auditor.Record(r.Context(), baseEvent); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	issueCtx, cancelIssue := context.WithTimeout(r.Context(), issueTimeout)
	issued, issueErr := s.issuers[grant.Provider].Issue(issueCtx, grant)
	cancelIssue()
	defer zero(issued.Value)
	defer zero(issued.RevokeHandle)
	issuedAt := s.now().UTC()
	if issueErr != nil || !validIssued(issued, grant, issuedAt) {
		if len(issued.RevokeHandle) > 0 {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), cleanupTimeout)
			_ = s.issuers[grant.Provider].Revoke(cleanupCtx, issued.RevokeHandle)
			cancel()
		}
		_ = s.auditor.Record(r.Context(), auditFor("acquire", "issue_failed", identity, requested, grant.Provider, leaseID, time.Time{}, s.now()))
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	issued.ExpiresAt = issued.ExpiresAt.UTC()
	record := LeaseRecord{LeaseID: leaseID, Principal: identity.PrincipalKey(), Provider: grant.Provider, RevokeHandle: append([]byte(nil), issued.RevokeHandle...), ExpiresAt: issued.ExpiresAt}
	if err := s.store.Commit(r.Context(), record); err != nil {
		zero(record.RevokeHandle)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), cleanupTimeout)
		_ = s.issuers[grant.Provider].Revoke(cleanupCtx, issued.RevokeHandle)
		cancel()
		_ = s.auditor.Record(r.Context(), auditFor("acquire", "commit_failed", identity, requested, grant.Provider, leaseID, issued.ExpiresAt, s.now()))
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	zero(record.RevokeHandle)
	if err := s.auditor.Record(r.Context(), auditFor("acquire", "issued", identity, requested, grant.Provider, leaseID, issued.ExpiresAt, s.now())); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), cleanupTimeout)
		if revokeErr := s.issuers[grant.Provider].Revoke(cleanupCtx, issued.RevokeHandle); revokeErr == nil {
			_ = s.store.MarkRevoked(cleanupCtx, leaseID, s.now().UTC())
		}
		cancel()
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	body := marshalLeaseResponse(leaseID, issued.Value, issued.ExpiresAt)
	_, _ = w.Write(body)
	zero(body)
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	leaseID := strings.TrimPrefix(r.URL.Path, "/v1/leases/")
	if !validLeaseID(leaseID) || strings.Contains(leaseID, "/") || r.URL.RawQuery != "" {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	record, err := s.store.Get(r.Context(), leaseID)
	if errors.Is(err, ErrLeaseNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	defer zero(record.RevokeHandle)
	if record.Principal != identity.PrincipalKey() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !record.RevokedAt.IsZero() || !s.now().Before(record.ExpiresAt) {
		if record.RevokedAt.IsZero() {
			_ = s.store.MarkRevoked(r.Context(), leaseID, s.now().UTC())
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	issuer := s.issuers[record.Provider]
	if issuer == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if err := s.auditor.Record(r.Context(), AuditEvent{Operation: "revoke", Result: "revoke_started", Identity: identity.PrincipalKey(), TaskID: identity.TaskID, Provider: record.Provider, LeaseID: leaseID, ExpiresAt: record.ExpiresAt, At: s.now().UTC()}); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	revokeCtx, cancelRevoke := context.WithTimeout(r.Context(), providerRevokeTimeout)
	revokeErr := issuer.Revoke(revokeCtx, record.RevokeHandle)
	cancelRevoke()
	if revokeErr != nil {
		_ = s.auditor.Record(r.Context(), AuditEvent{Operation: "revoke", Result: "revoke_failed", Identity: identity.PrincipalKey(), TaskID: identity.TaskID, Provider: record.Provider, LeaseID: leaseID, ExpiresAt: record.ExpiresAt, At: s.now().UTC()})
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if err := s.store.MarkRevoked(r.Context(), leaseID, s.now().UTC()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if err := s.auditor.Record(r.Context(), AuditEvent{Operation: "revoke", Result: "revoked", Identity: identity.PrincipalKey(), TaskID: identity.TaskID, Provider: record.Provider, LeaseID: leaseID, ExpiresAt: record.ExpiresAt, At: s.now().UTC()}); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	values := r.Header.Values("Authorization")
	r.Header.Del("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) <= len("Bearer ") {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return Identity{}, false
	}
	proof := strings.TrimPrefix(values[0], "Bearer ")
	if strings.TrimSpace(proof) != proof || len(proof) > 8192 || strings.ContainsAny(proof, "\r\n\t ") {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return Identity{}, false
	}
	identity, err := s.verifier.Verify(r.Context(), proof, s.audience)
	proof = ""
	if err != nil || !validClaim(identity.Issuer, 512) || !validClaim(identity.Subject, 512) || !validClaim(identity.WorkloadID, 256) || !validClaim(identity.TaskID, 256) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return Identity{}, false
	}
	return identity, true
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (LeaseRequest, bool) {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_request")
		return LeaseRequest{}, false
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentTypes[0], ";", 2)[0])); mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_request")
		return LeaseRequest{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return LeaseRequest{}, false
	}
	var wire wireRequest
	if err := strictjson.Decode(body, &wire); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return LeaseRequest{}, false
	}
	if wire.TTLSeconds < 0 || wire.TTLSeconds > int64(absoluteMaxTTL/time.Second) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return LeaseRequest{}, false
	}
	return LeaseRequest{TaskID: wire.TaskID, Audience: wire.Audience, Resource: wire.Resource, TargetOrigin: wire.TargetOrigin, Actions: append([]string(nil), wire.Actions...), TTL: time.Duration(wire.TTLSeconds) * time.Second, AuthMode: wire.AuthMode, AuthHeader: wire.AuthHeader}, true
}

func validateRequest(req LeaseRequest, maxTTL time.Duration) error {
	if !validClaim(req.TaskID, 256) || !validClaim(req.Audience, 512) || !ValidResourcePath(req.Resource) || req.TTL < time.Second || req.TTL%time.Second != 0 || req.TTL > maxTTL || len(req.Actions) == 0 || len(req.Actions) > 32 {
		return ErrDenied
	}
	canonical, err := canonicalOrigin(req.TargetOrigin)
	if err != nil || canonical != req.TargetOrigin {
		return ErrDenied
	}
	seen := make(map[string]struct{}, len(req.Actions))
	for _, action := range req.Actions {
		if !ValidHTTPMethod(action) {
			return ErrDenied
		}
		if _, ok := seen[action]; ok {
			return ErrDenied
		}
		seen[action] = struct{}{}
	}
	if req.AuthMode == "bearer" {
		if req.AuthHeader != "" {
			return ErrDenied
		}
	} else if req.AuthMode == "api_key_header" {
		if !safeAuthHeader(req.AuthHeader) {
			return ErrDenied
		}
	} else {
		return ErrDenied
	}
	return nil
}

// ValidResourcePath accepts one canonical absolute path with no query, fragment,
// traversal segment, encoded path separator, or encoded percent byte that
// could become ambiguous after repeated decoding.
func ValidResourcePath(resource string) bool {
	if resource == "" || len(resource) > 2048 || !strings.HasPrefix(resource, "/") || strings.ContainsAny(resource, "?#*\\\r\n\x00") || strings.Contains(resource, "//") {
		return false
	}
	u, err := url.ParseRequestURI(resource)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.EscapedPath() != resource {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	lower := strings.ToLower(resource)
	return !strings.Contains(lower, "%2f") && !strings.Contains(lower, "%5c") && !strings.Contains(lower, "%25")
}

// ValidHTTPMethod limits policy actions to methods handled by the Go Common
// target client. CONNECT and TRACE are excluded to avoid tunnel/debug use.
func ValidHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

func validGrant(grant Grant, req LeaseRequest, maxTTL time.Duration) bool {
	if !validClaim(grant.Provider, 128) || grant.MaxTTL < time.Second || grant.MaxTTL > maxTTL || grant.MaxTTL%time.Second != 0 {
		return false
	}
	approved := grant.Request
	if approved.TaskID != req.TaskID || approved.Audience != req.Audience || approved.Resource != req.Resource || approved.TargetOrigin != req.TargetOrigin || approved.AuthMode != req.AuthMode || approved.AuthHeader != req.AuthHeader || approved.TTL > req.TTL || approved.TTL < time.Second || len(approved.Actions) != len(req.Actions) {
		return false
	}
	for i := range req.Actions {
		if approved.Actions[i] != req.Actions[i] {
			return false
		}
	}
	return true
}

func validIssued(issued IssuedCredential, grant Grant, now time.Time) bool {
	if len(issued.Value) == 0 || len(issued.Value) > maxCredentialBytes || len(issued.RevokeHandle) == 0 || len(issued.RevokeHandle) > maxCredentialBytes || issued.ExpiresAt.IsZero() || !issued.ExpiresAt.After(now) || issued.ExpiresAt.After(now.Add(grant.Request.TTL)) {
		return false
	}
	for _, c := range issued.Value {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	if grant.Request.AuthMode == "bearer" {
		for _, c := range issued.Value {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~+/=", rune(c))) {
				return false
			}
		}
	}
	return true
}

func requestDigest(identity Identity, req LeaseRequest, provider string) (string, error) {
	body, err := json.Marshal(struct {
		Principal string      `json:"principal"`
		Provider  string      `json:"provider"`
		Request   wireRequest `json:"request"`
	}{Principal: identity.PrincipalKey(), Provider: provider, Request: wireRequest{TaskID: req.TaskID, Audience: req.Audience, Resource: req.Resource, TargetOrigin: req.TargetOrigin, Actions: req.Actions, TTLSeconds: int64(req.TTL / time.Second), AuthMode: req.AuthMode, AuthHeader: req.AuthHeader}})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:]), nil
}

func auditFor(operation, result string, identity Identity, req LeaseRequest, provider, leaseID string, expiresAt, at time.Time) AuditEvent {
	return AuditEvent{Operation: operation, Result: result, Identity: identity.PrincipalKey(), TaskID: identity.TaskID, Audience: req.Audience, Resource: req.Resource, Actions: append([]string(nil), req.Actions...), Provider: provider, LeaseID: leaseID, ExpiresAt: expiresAt.UTC(), At: at.UTC()}
}

func (s *Server) audit(ctx context.Context, event AuditEvent) { _ = s.auditor.Record(ctx, event) }

func marshalLeaseResponse(id string, credential []byte, expires time.Time) []byte {
	out := make([]byte, 0, len(id)+len(credential)+96)
	out = append(out, `{"lease_id":`...)
	out = appendJSONString(out, []byte(id))
	out = append(out, `,"credential":`...)
	out = appendJSONString(out, credential)
	out = append(out, `,"expires_at":`...)
	out = appendJSONString(out, []byte(expires.UTC().Format(time.RFC3339Nano)))
	out = append(out, '}', '\n')
	return out
}

func appendJSONString(dst, value []byte) []byte {
	dst = append(dst, '"')
	for _, b := range value {
		switch b {
		case '\\', '"':
			dst = append(dst, '\\', b)
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if b < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', "0123456789abcdef"[b>>4], "0123456789abcdef"[b&0x0f])
			} else {
				dst = append(dst, b)
			}
		}
	}
	return append(dst, '"')
}

func canonicalOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return "", ErrDenied
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", ErrDenied
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", ErrDenied
		}
		if n == 443 {
			port = ""
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return "https://" + host, nil
}

func safeAuthHeader(name string) bool {
	if name == "" || http.CanonicalHeaderKey(name) != name {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	lower := strings.ToLower(name)
	switch lower {
	case "authorization", "proxy-authorization", "proxy-authenticate", "www-authenticate", "cookie", "set-cookie", "host", "content-length", "transfer-encoding", "connection", "keep-alive", "proxy-connection", "te", "trailer", "upgrade", "accept", "accept-encoding", "content-type", "user-agent", "referer", "origin", "forwarded":
		return false
	}
	return !strings.HasPrefix(lower, "x-forwarded-") && !strings.HasPrefix(lower, "sec-")
}

func validIdempotencyKey(key string) bool {
	if len(key) != 32 {
		return false
	}
	decoded := make([]byte, 16)
	_, err := hex.Decode(decoded, []byte(key))
	return err == nil
}

func validLeaseID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func randomID() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func nonBlank(value string) bool { return value != "" && strings.TrimSpace(value) == value }

func validClaim(value string, max int) bool {
	if !nonBlank(value) || len(value) > max {
		return false
	}
	for _, c := range value {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, fmt.Sprintf("{\"error\":%q}\n", code))
}

var _ http.Handler = (*Server)(nil)
