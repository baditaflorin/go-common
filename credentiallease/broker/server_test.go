package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const (
	testProof  = "signed-workload-proof"
	testTask   = "task-17"
	testSecret = "provider-secret-value"
)

var testNow = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

type testVerifier struct{ identity Identity }

func (v testVerifier) Verify(_ context.Context, proof, audience string) (Identity, error) {
	if proof != testProof || audience != "credential-broker" {
		return Identity{}, ErrInvalidProof
	}
	return v.identity, nil
}

type testPolicy struct {
	change func(Grant) Grant
	maxTTL time.Duration
}

func (p testPolicy) Authorize(_ context.Context, _ Identity, req LeaseRequest) (Grant, error) {
	grant := Grant{Provider: "fake", Request: req, MaxTTL: p.maxTTL}
	if p.change != nil {
		grant = p.change(grant)
	}
	return grant, nil
}

type testIssuer struct {
	mu           sync.Mutex
	issues       int
	revocations  int
	now          time.Time
	value        string
	expires      time.Duration
	panicOnIssue bool
}

type advancingTestIssuer struct {
	now     *time.Time
	advance time.Duration
}

func (i advancingTestIssuer) Issue(_ context.Context, grant Grant) (IssuedCredential, error) {
	*i.now = i.now.Add(i.advance)
	return IssuedCredential{
		Value: []byte(testSecret), RevokeHandle: []byte("provider-revoke-handle"),
		ExpiresAt: i.now.Add(grant.Request.TTL),
	}, nil
}

func (advancingTestIssuer) Revoke(context.Context, []byte) error { return nil }

func (i *testIssuer) Issue(_ context.Context, grant Grant) (IssuedCredential, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.issues++
	if i.panicOnIssue {
		panic(testSecret)
	}
	expires := i.expires
	if expires == 0 {
		expires = grant.Request.TTL
	}
	return IssuedCredential{Value: []byte(i.value), RevokeHandle: []byte("provider-revoke-handle"), ExpiresAt: i.now.Add(expires)}, nil
}

func (i *testIssuer) Revoke(_ context.Context, handle []byte) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if string(handle) != "provider-revoke-handle" {
		return errors.New("unexpected revoke handle")
	}
	i.revocations++
	return nil
}

type testStore struct {
	mu           sync.Mutex
	reservations map[string]LeaseReservation
	leases       map[string]LeaseRecord
}

func newTestStore() *testStore {
	return &testStore{reservations: make(map[string]LeaseReservation), leases: make(map[string]LeaseRecord)}
}

func (s *testStore) Reserve(_ context.Context, reservation LeaseReservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.reservations[reservation.IdempotencyDigest]; ok {
		if previous.RequestDigest != reservation.RequestDigest || previous.Principal != reservation.Principal {
			return ErrIdempotencyConflict
		}
		return ErrIdempotencyUsed
	}
	s.reservations[reservation.IdempotencyDigest] = reservation
	return nil
}

func (s *testStore) Commit(_ context.Context, record LeaseRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.RevokeHandle = append([]byte(nil), record.RevokeHandle...)
	s.leases[record.LeaseID] = record
	return nil
}

func (s *testStore) Get(_ context.Context, leaseID string) (LeaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.leases[leaseID]
	if !ok {
		return LeaseRecord{}, ErrLeaseNotFound
	}
	record.RevokeHandle = append([]byte(nil), record.RevokeHandle...)
	return record, nil
}

func (s *testStore) MarkRevoked(_ context.Context, leaseID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.leases[leaseID]
	if !ok {
		return ErrLeaseNotFound
	}
	record.RevokedAt = at
	s.leases[leaseID] = record
	return nil
}

type testAuditor struct {
	mu     sync.Mutex
	events []AuditEvent
	fail   bool
}

func (a *testAuditor) Record(_ context.Context, event AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	if a.fail {
		return errors.New("audit backend unavailable")
	}
	return nil
}

func (a *testAuditor) snapshot() []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditEvent(nil), a.events...)
}

func newTestServer(t *testing.T) (*Server, *testIssuer, *testStore, *testAuditor) {
	t.Helper()
	issuer := &testIssuer{now: testNow, value: testSecret}
	store := newTestStore()
	auditor := &testAuditor{}
	server, err := New(Config{
		Audience: "credential-broker",
		MaxTTL:   5 * time.Minute,
		Verifier: testVerifier{identity: Identity{Issuer: "https://identity.example", Subject: "worker-1", WorkloadID: "go-app", TaskID: testTask}},
		Policy:   testPolicy{maxTTL: 3 * time.Minute},
		Store:    store,
		Auditor:  auditor,
		Issuers:  map[string]Issuer{"fake": issuer},
		Now:      func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return server, issuer, store, auditor
}

func acquireBody(taskID string, ttl int) []byte {
	body, _ := json.Marshal(map[string]any{
		"task_id": taskID, "audience": "catalog-api", "resource": "/items/42",
		"target_origin": "https://catalog.example", "actions": []string{http.MethodGet},
		"ttl_seconds": ttl, "auth_mode": "api_key_header", "auth_header": "X-Api-Key",
	})
	return body
}

func sendAcquire(server http.Handler, body []byte, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testProof)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	return response
}

func TestAcquireIssuesPolicyBoundLeaseAndKeepsSecretsOutOfAudit(t *testing.T) {
	server, issuer, store, auditor := newTestServer(t)
	response := sendAcquire(server, acquireBody(testTask, 120), "00112233445566778899aabbccddeeff")
	if response.Code != http.StatusCreated {
		t.Fatalf("acquire status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var lease struct {
		LeaseID    string    `json:"lease_id"`
		Credential string    `json:"credential"`
		ExpiresAt  time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Credential != testSecret || !lease.ExpiresAt.Equal(testNow.Add(2*time.Minute)) {
		t.Fatalf("unexpected lease response: %#v", lease)
	}
	record, err := store.Get(context.Background(), lease.LeaseID)
	if err != nil || string(record.RevokeHandle) != "provider-revoke-handle" {
		t.Fatalf("stored lease missing revoke handle: record=%#v err=%v", record, err)
	}
	issuer.mu.Lock()
	issues := issuer.issues
	issuer.mu.Unlock()
	if issues != 1 {
		t.Fatalf("provider issue count = %d", issues)
	}
	for _, event := range auditor.snapshot() {
		if bytes.Contains([]byte(event.Result), []byte(testSecret)) || event.TaskID != testTask {
			t.Fatalf("unexpected audit event contents: %#v", event)
		}
	}
	if bytes.Contains(response.Body.Bytes(), []byte(testProof)) {
		t.Fatal("workload proof appeared in response")
	}
}

func TestProviderExpiryIsBoundFromIssueCompletion(t *testing.T) {
	now := testNow
	issuer := advancingTestIssuer{now: &now, advance: 250 * time.Millisecond}
	server, err := New(Config{
		Audience: "credential-broker", MaxTTL: 5 * time.Minute,
		Verifier: testVerifier{identity: Identity{Issuer: "https://identity.example", Subject: "worker-1", WorkloadID: "go-app", TaskID: testTask}},
		Policy:   testPolicy{maxTTL: 3 * time.Minute}, Store: newTestStore(), Auditor: &testAuditor{},
		Issuers: map[string]Issuer{"fake": issuer}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	response := sendAcquire(server, acquireBody(testTask, 60), "00112233445566778899aabbccddeeff")
	if response.Code != http.StatusCreated {
		t.Fatalf("acquire status = %d; provider expiry measured from completion must be accepted", response.Code)
	}
	var lease struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &lease); err != nil {
		t.Fatal("decode lease response")
	}
	if !lease.ExpiresAt.Equal(now.Add(60 * time.Second)) {
		t.Fatalf("expires_at = %s, want issue completion + 60s", lease.ExpiresAt)
	}
}

func TestValidResourcePathRejectsRepeatedDecodeAmbiguity(t *testing.T) {
	for _, resource := range []string{"/items/%252Fadmin", "/items/%252E%252E/admin"} {
		if ValidResourcePath(resource) {
			t.Errorf("ambiguous resource path %q accepted", resource)
		}
	}
}

func TestAcquireRejectsTaskMismatchAndDoesNotIssue(t *testing.T) {
	server, issuer, _, _ := newTestServer(t)
	response := sendAcquire(server, acquireBody("another-task", 60), "00112233445566778899aabbccddeeff")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	if issuer.issues != 0 {
		t.Fatalf("issued %d credentials for task mismatch", issuer.issues)
	}
}

func TestAcquireRejectsIdempotencyReuseWithoutSecondIssue(t *testing.T) {
	server, issuer, _, _ := newTestServer(t)
	key := "00112233445566778899aabbccddeeff"
	if response := sendAcquire(server, acquireBody(testTask, 120), key); response.Code != http.StatusCreated {
		t.Fatalf("first acquire status = %d", response.Code)
	}
	if response := sendAcquire(server, acquireBody(testTask, 120), key); response.Code != http.StatusConflict {
		t.Fatalf("same-key replay status = %d, want 409", response.Code)
	}
	if response := sendAcquire(server, acquireBody(testTask, 90), key); response.Code != http.StatusConflict {
		t.Fatalf("different-body replay status = %d, want 409", response.Code)
	}
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	if issuer.issues != 1 {
		t.Fatalf("provider issued %d leases for one idempotency key", issuer.issues)
	}
}

func TestConcurrentDuplicateAcquireIssuesOnlyOneLease(t *testing.T) {
	server, issuer, _, _ := newTestServer(t)
	key := "00112233445566778899aabbccddeeff"
	start := make(chan struct{})
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			statuses <- sendAcquire(server, acquireBody(testTask, 120), key).Code
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	created, conflict := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected concurrent acquire status %d", status)
		}
	}
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	if created != 1 || conflict != 1 || issuer.issues != 1 {
		t.Fatalf("concurrent result created=%d conflict=%d issues=%d", created, conflict, issuer.issues)
	}
}

func TestPolicyReducesLeaseTTL(t *testing.T) {
	issuer := &testIssuer{now: testNow, value: testSecret}
	server, err := New(Config{
		Audience: "credential-broker", Verifier: testVerifier{identity: Identity{Issuer: "issuer", Subject: "subject", WorkloadID: "worker", TaskID: testTask}},
		Policy: testPolicy{maxTTL: 30 * time.Second}, Store: newTestStore(), Auditor: &testAuditor{},
		Issuers: map[string]Issuer{"fake": issuer}, Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	response := sendAcquire(server, acquireBody(testTask, 120), "00112233445566778899aabbccddeeff")
	if response.Code != http.StatusCreated {
		t.Fatalf("reduced TTL acquire status = %d body=%s", response.Code, response.Body.String())
	}
	var lease struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}
	if !lease.ExpiresAt.Equal(testNow.Add(30 * time.Second)) {
		t.Fatalf("expires_at = %s, want %s", lease.ExpiresAt, testNow.Add(30*time.Second))
	}
}

func TestPolicyCannotWidenRequestedScope(t *testing.T) {
	issuer := &testIssuer{now: testNow, value: testSecret}
	server, err := New(Config{
		Audience: "credential-broker", Verifier: testVerifier{identity: Identity{Issuer: "issuer", Subject: "subject", WorkloadID: "worker", TaskID: testTask}},
		Policy: testPolicy{maxTTL: 3 * time.Minute, change: func(g Grant) Grant { g.Request.Resource = "*"; return g }},
		Store:  newTestStore(), Auditor: &testAuditor{}, Issuers: map[string]Issuer{"fake": issuer},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := sendAcquire(server, acquireBody(testTask, 60), "00112233445566778899aabbccddeeff")
	if response.Code != http.StatusForbidden {
		t.Fatalf("scope-widening policy status = %d, want 403", response.Code)
	}
	if issuer.issues != 0 {
		t.Fatal("issuer called for widened grant")
	}
}

func TestInvalidProviderExpiryFailsClosed(t *testing.T) {
	server, issuer, _, _ := newTestServer(t)
	issuer.expires = 10 * time.Minute
	response := sendAcquire(server, acquireBody(testTask, 120), "00112233445566778899aabbccddeeff")
	if response.Code != http.StatusServiceUnavailable || bytes.Contains(response.Body.Bytes(), []byte(testSecret)) {
		t.Fatalf("invalid provider expiry response = %d %s", response.Code, response.Body.String())
	}
	if issuer.revocations != 1 {
		t.Fatalf("invalid provider expiry cleanup calls = %d, want 1", issuer.revocations)
	}
}

func TestProviderPanicDoesNotLeakPanicValue(t *testing.T) {
	server, issuer, _, _ := newTestServer(t)
	issuer.panicOnIssue = true
	response := sendAcquire(server, acquireBody(testTask, 120), "00112233445566778899aabbccddeeff")
	if response.Code != http.StatusInternalServerError || bytes.Contains(response.Body.Bytes(), []byte(testSecret)) {
		t.Fatalf("provider panic response = %d %s", response.Code, response.Body.String())
	}
}

func TestRevokeIsTaskBoundAndIdempotent(t *testing.T) {
	server, issuer, _, _ := newTestServer(t)
	response := sendAcquire(server, acquireBody(testTask, 120), "00112233445566778899aabbccddeeff")
	if response.Code != http.StatusCreated {
		t.Fatalf("acquire status = %d", response.Code)
	}
	var lease struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}
	wrongTaskServer, err := New(Config{
		Audience: "credential-broker", Verifier: testVerifier{identity: Identity{Issuer: "https://identity.example", Subject: "other", WorkloadID: "go-app", TaskID: "other-task"}},
		Policy: testPolicy{maxTTL: time.Minute}, Store: server.store, Auditor: server.auditor, Issuers: server.issuers, Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	wrong := httptest.NewRequest(http.MethodDelete, "/v1/leases/"+lease.LeaseID, nil)
	wrong.Header.Set("Authorization", "Bearer "+testProof)
	wrongResponse := httptest.NewRecorder()
	wrongTaskServer.ServeHTTP(wrongResponse, wrong)
	if wrongResponse.Code != http.StatusNoContent || issuer.revocations != 0 {
		t.Fatalf("cross-task revoke status=%d provider revocations=%d", wrongResponse.Code, issuer.revocations)
	}
	for n := 0; n < 2; n++ {
		req := httptest.NewRequest(http.MethodDelete, "/v1/leases/"+lease.LeaseID, nil)
		req.Header.Set("Authorization", "Bearer "+testProof)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("revoke #%d status = %d body=%s", n+1, recorder.Code, recorder.Body.String())
		}
	}
	if issuer.revocations != 1 {
		t.Fatalf("provider revoke calls = %d, want 1", issuer.revocations)
	}
}

func TestConstructorAndAuthenticationFailClosed(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty config error = %v", err)
	}
	server, _, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewReader(acquireBody(testTask, 60)))
	req.Header.Set("Authorization", "Bearer invalid")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "00112233445566778899aabbccddeeff")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid proof status = %d, want 401", response.Code)
	}
}

func TestEmptyIssuerMapSupportsDenyAllAndStillFailsClosed(t *testing.T) {
	cfg := Config{
		Audience: "credential-broker",
		MaxTTL:   time.Minute,
		Verifier: testVerifier{identity: Identity{Issuer: "https://identity.example", Subject: "worker-1", WorkloadID: "go-app", TaskID: testTask}},
		Policy:   testPolicy{maxTTL: time.Minute},
		Store:    newTestStore(),
		Auditor:  &testAuditor{},
		Issuers:  map[string]Issuer{},
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatalf("New() with an explicit empty issuer map: %v", err)
	}
	if _, err := New(Config{
		Audience: cfg.Audience, MaxTTL: cfg.MaxTTL, Verifier: cfg.Verifier,
		Policy: cfg.Policy, Store: cfg.Store, Auditor: cfg.Auditor,
	}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() with an absent issuer map = %v, want invalid config", err)
	}

	recorder := sendAcquire(server, acquireBody(testTask, 30), "00112233445566778899aabbccddeeff")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("grant without a configured issuer status = %d, want 403", recorder.Code)
	}
}

func TestDuplicateSecurityHeadersAreRejected(t *testing.T) {
	server, issuer, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewReader(acquireBody(testTask, 60)))
	req.Header.Add("Authorization", "Bearer "+testProof)
	req.Header.Add("Authorization", "Bearer "+testProof)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "00112233445566778899aabbccddeeff")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized || issuer.issues != 0 {
		t.Fatalf("duplicate authorization status=%d issues=%d", response.Code, issuer.issues)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewReader(acquireBody(testTask, 60)))
	req.Header.Set("Authorization", "Bearer "+testProof)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("Idempotency-Key", "00112233445566778899aabbccddeeff")
	req.Header.Add("Idempotency-Key", "ffeeddccbbaa99887766554433221100")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest || issuer.issues != 0 {
		t.Fatalf("duplicate idempotency status=%d issues=%d", response.Code, issuer.issues)
	}
}

func TestOversizedRequestIsRejected(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	body := bytes.Repeat([]byte("x"), maxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/leases", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testProof)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "00112233445566778899aabbccddeeff")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized request status = %d, want 400", response.Code)
	}
}

func TestCredentialAndRevokeHandleFormattingRedacts(t *testing.T) {
	issued := IssuedCredential{Value: []byte(testSecret), RevokeHandle: []byte("private-handle")}
	encoded, err := json.Marshal(issued)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(testSecret)) || bytes.Contains(encoded, []byte("private-handle")) {
		t.Fatalf("issued credential leaked through JSON formatting: %s", encoded)
	}
	record, err := json.Marshal(LeaseRecord{LeaseID: "lease", RevokeHandle: []byte("private-handle")})
	if err != nil || bytes.Contains(record, []byte("private-handle")) {
		t.Fatalf("lease record formatting leaked handle: %s err=%v", record, err)
	}
}

func TestErrorResponseDoesNotReflectBackendText(t *testing.T) {
	server, issuer, _, _ := newTestServer(t)
	issuer.expires = 10 * time.Minute
	response := sendAcquire(server, acquireBody(testTask, 120), "00112233445566778899aabbccddeeff")
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(testSecret)) {
		t.Fatal("secret appeared in error response")
	}
}
