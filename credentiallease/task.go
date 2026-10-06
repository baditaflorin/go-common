package credentiallease

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/spiffe"
)

// ErrTaskMismatch reports an attempted lease request for a task other than the
// broker-created task represented by Task.
var ErrTaskMismatch = errors.New("credentiallease: task mismatch")

// ErrTaskClosed reports that the broker-created task is closed or expired.
var ErrTaskClosed = errors.New("credentiallease: task closed")

// ErrTaskCreateFailed is returned when the broker cannot create a task. It
// deliberately does not include response bodies or credential material.
var ErrTaskCreateFailed = errors.New("credentiallease: task creation failed")

// ErrTaskCloseFailed is returned when the broker cannot close a task.
var ErrTaskCloseFailed = errors.New("credentiallease: task close failed")

// BootstrapCredentialSource provides the calling workload's existing service
// credential. It must not return a provider credential or broker admin token.
type BootstrapCredentialSource interface {
	Credential(context.Context) (string, error)
}

// BootstrapCredentialFunc adapts a function to BootstrapCredentialSource.
type BootstrapCredentialFunc func(context.Context) (string, error)

// Credential implements BootstrapCredentialSource.
func (f BootstrapCredentialFunc) Credential(ctx context.Context) (string, error) {
	return f(ctx)
}

func setBrokerAccessHeader(ctx context.Context, source BootstrapCredentialSource, req *http.Request) error {
	if ctx == nil || source == nil || req == nil {
		return ErrIdentityUnavailable
	}
	credential, err := source.Credential(ctx)
	if err != nil || !validBootstrapCredential(credential) {
		credential = ""
		return ErrIdentityUnavailable
	}
	req.Header.Set("X-API-Key", credential)
	credential = ""
	return nil
}

// TaskManagerConfig configures broker task creation. The service credential is
// sent only over the validated HTTPS endpoint and is never logged by this
// package.
type TaskManagerConfig struct {
	Endpoint         string
	BrokerAudience   string
	Bootstrap        BootstrapCredentialSource
	Identity         IdentityTokenSource
	IdentityAudience string
	SPIFFEClient     *spiffe.HTTPClient
	TLSConfig        *tls.Config
	RequestTimeout   time.Duration
	TaskTTL          time.Duration
}

func (TaskManagerConfig) String() string { return "credentiallease.TaskManagerConfig([REDACTED])" }

func (c TaskManagerConfig) GoString() string { return c.String() }

func (c TaskManagerConfig) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, c.String())
}

func (c TaskManagerConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Config string `json:"config"`
	}{Config: "[REDACTED]"})
}

// TaskManager authenticates service workloads and asks the broker to create a
// broker-owned task identity. It uses exactly one bootstrap API-key source or
// SPIFFE mTLS client. It is safe for concurrent use when its selected source
// and transport are safe for concurrent use.
type TaskManager struct {
	endpoint         string
	brokerAudience   string
	bootstrap        BootstrapCredentialSource
	identity         IdentityTokenSource
	identityAudience string
	http             interface {
		Do(*http.Request) (*http.Response, error)
		CloseIdleConnections()
	}
	spiffeClient   *spiffe.HTTPClient
	closeOnce      sync.Once
	closeErr       error
	requestTimeout time.Duration
	taskTTL        time.Duration
	tlsConfig      *tls.Config
}

// NewTaskManager validates configuration and creates an HTTPS-only client.
// The transport does not use environment proxy settings and never follows
// redirects, so workload proofs cannot be forwarded to another host.
func NewTaskManager(cfg TaskManagerConfig) (*TaskManager, error) {
	endpoint, err := validateEndpoint(cfg.Endpoint)
	if err != nil || !nonBlankExact(cfg.BrokerAudience) || len(cfg.BrokerAudience) > 512 ||
		(cfg.SPIFFEClient == nil && cfg.Bootstrap == nil) ||
		(cfg.SPIFFEClient != nil && (cfg.Bootstrap != nil || cfg.Identity != nil)) ||
		(cfg.Identity != nil && (!nonBlankExact(cfg.IdentityAudience) || len(cfg.IdentityAudience) > 512)) ||
		(cfg.Identity == nil && cfg.IdentityAudience != "") || (cfg.SPIFFEClient != nil && cfg.TLSConfig != nil) {
		return nil, ErrInvalidConfig
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = 3 * time.Second
	}
	if requestTimeout < time.Second || requestTimeout > 15*time.Second {
		return nil, ErrInvalidConfig
	}
	taskTTL := cfg.TaskTTL
	if taskTTL == 0 {
		taskTTL = 10 * time.Minute
	}
	if taskTTL < time.Minute || taskTTL > 15*time.Minute || taskTTL%time.Second != 0 {
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
	if cfg.SPIFFEClient != nil {
		tlsConfig = nil
	}
	var storedTLSConfig *tls.Config
	if tlsConfig != nil {
		storedTLSConfig = tlsConfig.Clone()
	}
	var httpClient interface {
		Do(*http.Request) (*http.Response, error)
		CloseIdleConnections()
	}
	if cfg.SPIFFEClient != nil {
		httpClient = cfg.SPIFFEClient
	} else {
		transport := &http.Transport{
			Proxy:                  nil,
			TLSClientConfig:        tlsConfig,
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  requestTimeout,
			IdleConnTimeout:        30 * time.Second,
			MaxIdleConns:           4,
			MaxIdleConnsPerHost:    2,
			MaxConnsPerHost:        4,
			MaxResponseHeaderBytes: 16 << 10,
			DisableCompression:     true,
		}
		httpClient = &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &TaskManager{
		endpoint:         endpoint,
		brokerAudience:   strings.TrimSpace(cfg.BrokerAudience),
		bootstrap:        cfg.Bootstrap,
		identity:         cfg.Identity,
		identityAudience: cfg.IdentityAudience,
		spiffeClient:     cfg.SPIFFEClient,
		http:             httpClient,
		requestTimeout:   requestTimeout,
		taskTTL:          taskTTL,
		tlsConfig:        storedTLSConfig,
	}, nil
}

// BeginTask asks the broker to authenticate this service workload, generate a
// random task ID, and return a short-lived signed proof. The caller cannot
// choose the task ID.
func (m *TaskManager) BeginTask(ctx context.Context) (*Task, error) {
	if m == nil || m.http == nil || ctx == nil || (m.bootstrap == nil && m.spiffeClient == nil) {
		return nil, ErrInvalidConfig
	}
	key := ""
	token := ""
	if m.spiffeClient == nil {
		identityCtx, cancel := context.WithTimeout(ctx, m.requestTimeout)
		var err error
		key, err = m.bootstrap.Credential(identityCtx)
		if err != nil || !validBootstrapCredential(key) {
			cancel()
			key = ""
			return nil, ErrIdentityUnavailable
		}
		if m.identity != nil {
			token, err = m.identity.Token(identityCtx, m.identityAudience)
		}
		cancel()
		if err != nil || (m.identity != nil && !validBearerToken(token)) {
			key = ""
			token = ""
			return nil, ErrIdentityUnavailable
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, m.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, m.endpoint+"/v1/tasks", bytes.NewReader(nil))
	if err != nil {
		key = ""
		token = ""
		return nil, ErrTaskCreateFailed
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	resp, err := m.http.Do(req)
	req.Header.Del("X-API-Key")
	req.Header.Del("Authorization")
	key = ""
	token = ""
	if err != nil {
		return nil, ErrTaskCreateFailed
	}
	defer resp.Body.Close()
	if resp.ContentLength > 16<<10 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return nil, ErrTaskCreateFailed
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if readErr != nil || len(raw) > 16<<10 {
		zero(raw)
		return nil, ErrTaskCreateFailed
	}
	defer zero(raw)
	var out struct {
		TaskID    string    `json:"task_id"`
		Proof     string    `json:"proof"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	decodeErr := dec.Decode(&out)
	var trailing any
	trailingErr := dec.Decode(&trailing)
	if resp.StatusCode != http.StatusCreated || decodeErr != nil || trailingErr != io.EOF || !validLeaseID(out.TaskID) || !validBearerToken(out.Proof) || out.ExpiresAt.IsZero() || !out.ExpiresAt.After(time.Now()) || out.ExpiresAt.After(time.Now().Add(m.taskTTL)) {
		m.discardTask(ctx, out.TaskID, out.Proof)
		out.Proof = ""
		return nil, ErrTaskCreateFailed
	}
	proof := []byte(out.Proof)
	out.Proof = ""
	return &Task{manager: m, id: out.TaskID, proof: proof, expiresAt: out.ExpiresAt, done: make(chan struct{})}, nil
}

// Close releases the rotating SPIFFE identity source, if configured.
func (m *TaskManager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		if m.spiffeClient != nil {
			m.closeErr = m.spiffeClient.Close()
		} else if m.http != nil {
			m.http.CloseIdleConnections()
		}
	})
	return m.closeErr
}

// WithTask creates one broker-owned task identity, runs fn with that task,
// then closes the task even when fn returns an error or panics. Close errors
// are joined with callback errors; a panic still propagates after cleanup.
func (m *TaskManager) WithTask(ctx context.Context, fn func(context.Context, *Task) error) (retErr error) {
	if m == nil || ctx == nil || fn == nil {
		return ErrInvalidConfig
	}
	task, err := m.BeginTask(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := task.Close(ctx); closeErr != nil {
			if retErr == nil {
				retErr = closeErr
			} else {
				retErr = errors.Join(retErr, closeErr)
			}
		}
	}()
	return fn(ctx, task)
}

// discardTask closes a broker task if the create endpoint returned enough
// authenticated metadata to clean up an otherwise unusable response.
func (m *TaskManager) discardTask(ctx context.Context, id, proof string) {
	if m == nil || m.http == nil || !validLeaseID(id) || !validBearerToken(proof) || ctx == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), min(m.requestTimeout, 3*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(cleanupCtx, http.MethodDelete, m.endpoint+"/v1/tasks/"+id, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+proof)
	req.Header.Set("Cache-Control", "no-store")
	if m.spiffeClient == nil {
		if err := setBrokerAccessHeader(cleanupCtx, m.bootstrap, req); err != nil {
			req.Header.Del("Authorization")
			return
		}
	}
	resp, err := m.http.Do(req)
	req.Header.Del("Authorization")
	req.Header.Del("X-API-Key")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
}

// CloseIdleConnections closes idle broker connections.
func (m *TaskManager) CloseIdleConnections() {
	if m != nil && m.http != nil {
		m.http.CloseIdleConnections()
	}
}

// Task represents one broker-created task. Its proof is never exposed through
// accessors, formatting, or JSON serialization.
type Task struct {
	manager   *TaskManager
	id        string
	expiresAt time.Time

	mu      sync.RWMutex
	proof   []byte
	done    chan struct{}
	closing bool
	closed  bool
}

func (t *Task) String() string { return "credentiallease.Task([REDACTED])" }

func (t *Task) GoString() string { return t.String() }

func (t *Task) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, t.String())
}

func (t *Task) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Task string `json:"task"`
	}{Task: "[REDACTED]"})
}

// ID returns the non-secret task identifier created by the broker.
func (t *Task) ID() string {
	if t == nil {
		return ""
	}
	return t.id
}

// ExpiresAt returns the broker-reported task proof expiration.
func (t *Task) ExpiresAt() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.expiresAt
}

// Client creates a lease client whose identity source is bound to this task.
// The resulting TaskClient rejects requests that contain another task ID.
func (t *Task) Client(cfg Config) (*TaskClient, error) {
	if t == nil || t.manager == nil {
		return nil, ErrInvalidConfig
	}
	t.mu.RLock()
	closed := t.closing || t.closed
	t.mu.RUnlock()
	if closed {
		return nil, ErrTaskClosed
	}
	cfg.Endpoint = t.manager.endpoint
	cfg.BrokerAudience = t.manager.brokerAudience
	cfg.Identity = taskTokenSource{task: t}
	cfg.BrokerAccess = t.manager.bootstrap
	cfg.TLSConfig = t.manager.tlsConfig
	cfg.SPIFFEClient = t.manager.spiffeClient
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &TaskClient{task: t, client: client}, nil
}

// Close closes the broker task. Existing leases can still use their task proof
// for DELETE/revoke, but the broker policy must reject new acquisitions.
// Calling Close repeatedly is safe.
func (t *Task) Close(ctx context.Context) error {
	if t == nil || t.manager == nil || ctx == nil {
		return ErrInvalidConfig
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	if !t.closing {
		t.closing = true
		if t.done != nil {
			close(t.done)
		}
	}
	proof := append([]byte(nil), t.proof...)
	t.mu.Unlock()
	if len(proof) == 0 {
		return ErrTaskClosed
	}
	if !time.Now().Before(t.expiresAt) {
		zero(proof)
		t.markClosed()
		return nil // the signed task proof has expired; no new lease can use it.
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), min(t.manager.requestTimeout, 5*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(closeCtx, http.MethodDelete, t.manager.endpoint+"/v1/tasks/"+t.id, nil)
	if err != nil {
		zero(proof)
		return ErrTaskCloseFailed
	}
	req.Header.Set("Authorization", "Bearer "+string(proof))
	req.Header.Set("Cache-Control", "no-store")
	if t.manager.spiffeClient == nil {
		if err := setBrokerAccessHeader(closeCtx, t.manager.bootstrap, req); err != nil {
			req.Header.Del("Authorization")
			zero(proof)
			return ErrTaskCloseFailed
		}
	}
	resp, err := t.manager.http.Do(req)
	req.Header.Del("Authorization")
	req.Header.Del("X-API-Key")
	zero(proof)
	if err != nil {
		return ErrTaskCloseFailed
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusNoContent {
		return ErrTaskCloseFailed
	}
	t.markClosed()
	return nil
}

func (t *Task) markClosed() {
	t.mu.Lock()
	t.closed = true
	zero(t.proof)
	t.proof = nil
	t.mu.Unlock()
}

type taskTokenSource struct {
	task *Task
}

func (s taskTokenSource) Token(ctx context.Context, audience string) (string, error) {
	if s.task == nil || ctx == nil || audience != s.task.manager.brokerAudience {
		return "", ErrIdentityUnavailable
	}
	s.task.mu.RLock()
	defer s.task.mu.RUnlock()
	if s.task.closing || s.task.closed || len(s.task.proof) == 0 || !time.Now().Before(s.task.expiresAt) {
		return "", ErrTaskClosed
	}
	return string(s.task.proof), nil
}

// TaskClient binds ordinary lease operations to exactly one broker-created
// task, so callers do not pass identity proofs or choose task IDs.
type TaskClient struct {
	task   *Task
	client *Client
}

func (c *TaskClient) WithLease(ctx context.Context, request Request, fn func(context.Context, *Lease) error) error {
	if c == nil || c.task == nil || c.client == nil || ctx == nil || fn == nil {
		return ErrInvalidConfig
	}
	if request.TaskID != c.task.id {
		return ErrTaskMismatch
	}
	c.task.mu.RLock()
	closing := c.task.closing || c.task.closed
	done := c.task.done
	c.task.mu.RUnlock()
	if closing {
		return ErrTaskClosed
	}
	taskCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(channelContext{done: done}, cancel)
	defer func() {
		stop()
		cancel()
	}()
	return c.client.WithLease(taskCtx, request, fn)
}

type channelContext struct {
	done <-chan struct{}
}

func (c channelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c channelContext) Done() <-chan struct{}       { return c.done }
func (c channelContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}
func (c channelContext) Value(any) any { return nil }

func (c *TaskClient) CloseIdleConnections() {
	if c != nil && c.client != nil {
		c.client.CloseIdleConnections()
	}
}

func validBootstrapCredential(value string) bool {
	return value != "" && len(value) <= 8192 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}
