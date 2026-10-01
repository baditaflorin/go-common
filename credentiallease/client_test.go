package credentiallease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureCredential = "fixture-credential-value"

func TestWithLeaseAttachesAndRevokesAndRedacts(t *testing.T) {
	var acquireCalls, revokeCalls atomic.Int32
	var observed []Event
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != fixtureCredential {
			t.Error("lease credential did not reach the bound target")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == protocolPath:
			acquireCalls.Add(1)
			if r.Header.Get("X-API-Key") != "broker-access-key" {
				t.Error("acquire did not use the gateway-scoped broker key")
			}
			if got := r.Header.Get("Authorization"); got != "Bearer workload-proof" {
				t.Errorf("unexpected broker authorization header")
			}
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("missing per-call idempotency key")
			}
			var got acquireRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if got.TaskID != "task-42" || got.Audience != "catalog-api" || got.Resource != "/resource" || got.TargetOrigin != target.URL || got.AuthMode != AuthAPIKeyHeader || !strings.EqualFold(got.AuthHeader, "X-API-Key") || got.TTLSeconds != 120 {
				t.Errorf("unexpected lease request metadata: %#v", got)
			}
			if len(got.Actions) != 1 || got.Actions[0] != http.MethodGet {
				t.Errorf("unexpected actions: %#v", got.Actions)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(leaseResponseFixture{
				LeaseID:    "lease_abc123",
				Credential: fixtureCredential,
				ExpiresAt:  time.Now().Add(90 * time.Second).UTC(),
			})
		case r.Method == http.MethodDelete && r.URL.Path == protocolPath+"/lease_abc123":
			revokeCalls.Add(1)
			if r.Header.Get("X-API-Key") != "broker-access-key" {
				t.Error("revoke did not use the gateway-scoped broker key")
			}
			if got := r.Header.Get("Authorization"); got != "Bearer workload-proof" {
				t.Errorf("unexpected revoke authorization header")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected broker route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(t, server, IdentityTokenSourceFunc(func(context.Context, string) (string, error) {
		return "workload-proof", nil
	}), ObserverFunc(func(event Event) { observed = append(observed, event) }))
	request := testRequest()
	request.TargetOrigin = target.URL
	var retained *Lease
	err := client.WithLease(context.Background(), request, func(ctx context.Context, lease *Lease) error {
		retained = lease
		if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(lease.ExpiresAt()) {
			t.Error("callback context is not bounded by broker expiry")
		}
		outbound, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.URL+"/resource", nil)
		response, err := lease.Do(target.Client(), outbound)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Errorf("target status = %d, want 204", response.StatusCode)
		}
		if outbound.Header.Get("X-API-Key") != "" {
			t.Error("lease.Do mutated the caller's request headers")
		}
		for _, rendered := range []string{fmt.Sprintf("%v", lease), fmt.Sprintf("%+v", lease), fmt.Sprintf("%#v", lease), fmt.Sprintf("%#v", *lease)} {
			if strings.Contains(rendered, fixtureCredential) {
				t.Error("formatted Lease exposed credential")
			}
		}
		encoded, err := json.Marshal(lease)
		if err != nil || strings.Contains(string(encoded), fixtureCredential) {
			t.Error("JSON Lease output exposed credential")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithLease returned error: %v", err)
	}
	if acquireCalls.Load() != 1 || revokeCalls.Load() != 1 {
		t.Fatalf("acquire/revoke calls = %d/%d, want 1/1", acquireCalls.Load(), revokeCalls.Load())
	}
	expiredRequest, _ := http.NewRequest(http.MethodGet, target.URL+"/resource", nil)
	if _, err := retained.Do(target.Client(), expiredRequest); !errors.Is(err, ErrLeaseClosed) {
		t.Fatalf("Do after callback = %v, want ErrLeaseClosed", err)
	}
	if len(observed) != 3 || observed[0].Operation != "acquire" || observed[1].Operation != "use" || observed[1].Result != "ok" || observed[1].Status != http.StatusNoContent || observed[2].Operation != "revoke" {
		t.Fatalf("unexpected lifecycle events: %#v", observed)
	}
}

func TestWithLeaseRejectsTTLBeforeIdentityLookup(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	var identityCalls atomic.Int32
	client := testClient(t, server, IdentityTokenSourceFunc(func(context.Context, string) (string, error) {
		identityCalls.Add(1)
		return "workload-proof", nil
	}), nil)
	request := testRequest()
	request.TTL = 16 * time.Minute
	err := client.WithLease(context.Background(), request, func(context.Context, *Lease) error { return nil })
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("WithLease error = %v, want ErrInvalidRequest", err)
	}
	if identityCalls.Load() != 0 {
		t.Fatalf("identity calls = %d, want 0", identityCalls.Load())
	}
}

func TestWithLeaseRejectsWildcardResourceBeforeIdentityLookup(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	var identityCalls atomic.Int32
	client := testClient(t, server, IdentityTokenSourceFunc(func(context.Context, string) (string, error) {
		identityCalls.Add(1)
		return "workload-proof", nil
	}), nil)
	request := testRequest()
	request.Resource = "*"
	err := client.WithLease(context.Background(), request, func(context.Context, *Lease) error { return nil })
	if !errors.Is(err, ErrInvalidRequest) || identityCalls.Load() != 0 {
		t.Fatalf("error=%v identity calls=%d; want invalid request and 0 identity calls", err, identityCalls.Load())
	}
}

func TestWithLeaseRevokesAfterCancellation(t *testing.T) {
	var revoked atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(leaseResponseFixture{LeaseID: "lease_cancel", Credential: fixtureCredential, ExpiresAt: time.Now().Add(time.Minute)})
			return
		}
		if r.Method == http.MethodDelete {
			revoked.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	var identityCalls atomic.Int32
	client := testClient(t, server, IdentityTokenSourceFunc(func(ctx context.Context, _ string) (string, error) {
		identityCalls.Add(1)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "workload-proof", nil
	}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	err := client.WithLease(ctx, testRequest(), func(ctx context.Context, _ *Lease) error {
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithLease error = %v, want context.Canceled", err)
	}
	if !revoked.Load() || identityCalls.Load() != 1 {
		t.Fatalf("revoked=%t identity calls=%d, want true and 1", revoked.Load(), identityCalls.Load())
	}
}

func TestWithLeaseRefreshesProofAfterRevokeUnauthorized(t *testing.T) {
	var identityCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.Header.Get("Authorization") != "Bearer old-proof" {
				t.Error("acquire did not use initial workload proof")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(leaseResponseFixture{LeaseID: "lease_refresh", Credential: fixtureCredential, ExpiresAt: time.Now().Add(time.Minute)})
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer old-proof":
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer fresh-proof":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("revoke used an unknown workload proof")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	client := testClient(t, server, IdentityTokenSourceFunc(func(context.Context, string) (string, error) {
		if identityCalls.Add(1) == 1 {
			return "old-proof", nil
		}
		return "fresh-proof", nil
	}), nil)
	err := client.WithLease(context.Background(), testRequest(), func(context.Context, *Lease) error { return nil })
	if err != nil || identityCalls.Load() != 2 {
		t.Fatalf("error=%v identity calls=%d; wanted successful refreshed revoke", err, identityCalls.Load())
	}
}

func TestWithLeaseRevokesAfterCallbackPanic(t *testing.T) {
	var revoked atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(leaseResponseFixture{LeaseID: "lease_panic", Credential: fixtureCredential, ExpiresAt: time.Now().Add(time.Minute)})
			return
		}
		revoked.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := testClient(t, server, IdentityTokenSourceFunc(func(context.Context, string) (string, error) { return "workload-proof", nil }), nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("callback panic did not propagate")
			}
		}()
		_ = client.WithLease(context.Background(), testRequest(), func(context.Context, *Lease) error { panic("callback failed") })
	}()
	if !revoked.Load() {
		t.Error("lease was not revoked after callback panic")
	}
}

func TestWithLeaseCleansUpInvalidExpiry(t *testing.T) {
	var revoked atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(leaseResponseFixture{LeaseID: "lease_bad_expiry", Credential: fixtureCredential, ExpiresAt: time.Now().Add(20 * time.Minute)})
			return
		}
		revoked.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := testClient(t, server, IdentityTokenSourceFunc(func(context.Context, string) (string, error) { return "workload-proof", nil }), nil)
	err := client.WithLease(context.Background(), testRequest(), func(context.Context, *Lease) error {
		t.Error("callback ran with invalid lease metadata")
		return nil
	})
	if !errors.Is(err, ErrInvalidLease) || !revoked.Load() {
		t.Fatalf("error=%v revoked=%t; want invalid lease and cleanup", err, revoked.Load())
	}
}

func TestWithLeaseReportsRevokeFailureAlongsideCallbackFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(leaseResponseFixture{LeaseID: "lease_revoke_error", Credential: fixtureCredential, ExpiresAt: time.Now().Add(time.Minute)})
			return
		}
		http.Error(w, "internal detail must not be surfaced", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := testClient(t, server, IdentityTokenSourceFunc(func(context.Context, string) (string, error) { return "workload-proof", nil }), nil)
	callbackErr := errors.New("consumer callback failed")
	err := client.WithLease(context.Background(), testRequest(), func(context.Context, *Lease) error { return callbackErr })
	if !errors.Is(err, callbackErr) || !errors.Is(err, ErrRevokeFailed) || strings.Contains(err.Error(), "internal detail") {
		t.Fatalf("unexpected combined error: %v", err)
	}
}

func TestClientRejectsInsecureConfigAndAuthHeaders(t *testing.T) {
	if _, err := NewClient(Config{Endpoint: "http://broker.invalid", BrokerAudience: "broker", Identity: IdentityTokenSourceFunc(func(context.Context, string) (string, error) { return "proof", nil })}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("cleartext endpoint error = %v", err)
	}
	if err := APIKeyHeader("Authorization").validate(); !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("Authorization API-key placement error = %v", err)
	}
	if err := APIKeyHeader("Cookie").validate(); !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("Cookie API-key placement error = %v", err)
	}
	config := Config{
		Endpoint:       "https://broker.invalid",
		BrokerAudience: "broker",
		Identity:       storedIdentity{value: fixtureCredential},
		BrokerAccess:   BootstrapCredentialFunc(func(context.Context) (string, error) { return "broker-key", nil }),
	}
	for _, rendered := range []string{fmt.Sprintf("%v", config), fmt.Sprintf("%+v", config), fmt.Sprintf("%#v", config)} {
		if strings.Contains(rendered, fixtureCredential) {
			t.Error("formatted Config exposed identity data")
		}
	}
	encoded, err := json.Marshal(config)
	if err != nil || strings.Contains(string(encoded), fixtureCredential) {
		t.Error("JSON Config output exposed identity data")
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient for formatting check: %v", err)
	}
	for _, rendered := range []string{fmt.Sprintf("%#v", client), fmt.Sprintf("%#v", *client)} {
		if strings.Contains(rendered, fixtureCredential) {
			t.Error("formatted Client exposed identity data")
		}
	}
}

func TestClientDoesNotFollowBrokerRedirect(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer target.Close()
	broker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+protocolPath)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer broker.Close()
	client := testClient(t, broker, IdentityTokenSourceFunc(func(context.Context, string) (string, error) { return "workload-proof", nil }), nil)
	err := client.WithLease(context.Background(), testRequest(), func(context.Context, *Lease) error { return nil })
	if !errors.Is(err, ErrBrokerUnavailable) || redirected.Load() != 0 {
		t.Fatalf("error=%v redirected calls=%d; want no redirect follow", err, redirected.Load())
	}
}

func TestLeaseBindsTargetOriginAndStopsTargetRedirect(t *testing.T) {
	var redirected atomic.Int32
	redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer redirectTarget.Close()
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirectTarget.URL+"/other")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer target.Close()
	broker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(leaseResponseFixture{LeaseID: "lease_target_guard", Credential: fixtureCredential, ExpiresAt: time.Now().Add(time.Minute)})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer broker.Close()
	client := testClient(t, broker, IdentityTokenSourceFunc(func(context.Context, string) (string, error) { return "workload-proof", nil }), nil)
	request := testRequest()
	request.TargetOrigin = target.URL
	err := client.WithLease(context.Background(), request, func(ctx context.Context, lease *Lease) error {
		wrong, _ := http.NewRequestWithContext(ctx, http.MethodGet, redirectTarget.URL+"/resource", nil)
		if _, err := lease.Do(redirectTarget.Client(), wrong); !errors.Is(err, ErrTargetMismatch) {
			return fmt.Errorf("wrong origin error = %v", err)
		}
		wrongPath, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.URL+"/other", nil)
		if _, err := lease.Do(target.Client(), wrongPath); !errors.Is(err, ErrResourceMismatch) {
			return fmt.Errorf("wrong path error = %v", err)
		}
		wrongMethod, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.URL+"/resource", nil)
		if _, err := lease.Do(target.Client(), wrongMethod); !errors.Is(err, ErrResourceMismatch) {
			return fmt.Errorf("wrong method error = %v", err)
		}
		withQuery, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.URL+"/resource?next=other", nil)
		if _, err := lease.Do(target.Client(), withQuery); !errors.Is(err, ErrResourceMismatch) {
			return fmt.Errorf("query escape error = %v", err)
		}
		hostOverride, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.URL+"/resource", nil)
		hostOverride.Host = "attacker.example"
		if _, err := lease.Do(target.Client(), hostOverride); !errors.Is(err, ErrTargetMismatch) {
			return fmt.Errorf("host override error = %v", err)
		}
		right, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.URL+"/resource", nil)
		response, err := lease.Do(target.Client(), right)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusTemporaryRedirect {
			return fmt.Errorf("target redirect response status = %d", response.StatusCode)
		}
		return nil
	})
	if err != nil || redirected.Load() != 0 {
		t.Fatalf("error=%v redirect target calls=%d", err, redirected.Load())
	}
}

func TestResourcePathAndActionMustBeExactAndCanonical(t *testing.T) {
	for _, resource := range []string{"/v1/items/42", "/v1/a%20b"} {
		if !validResourcePath(resource) {
			t.Errorf("canonical resource path %q rejected", resource)
		}
	}
	for _, resource := range []string{"", "record:7", "*", "/v1/items?next=42", "/v1/items#part", "/v1/../admin", "/v1//items", "/v1/items%2Fadmin", "/v1/items%5cadmin", "/v1/items%252Fadmin", "/v1/items*"} {
		if validResourcePath(resource) {
			t.Errorf("unsafe resource path %q accepted", resource)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if !validHTTPMethod(method) {
			t.Errorf("valid HTTP method %q rejected", method)
		}
	}
	for _, method := range []string{"read", "get", http.MethodConnect, http.MethodTrace, "*"} {
		if validHTTPMethod(method) {
			t.Errorf("invalid HTTP method %q accepted", method)
		}
	}
}

func TestObserverPanicDoesNotSkipRevocation(t *testing.T) {
	var revoked atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(leaseResponseFixture{LeaseID: "lease_observer_panic", Credential: fixtureCredential, ExpiresAt: time.Now().Add(time.Minute)})
			return
		}
		revoked.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := testClient(t, server, IdentityTokenSourceFunc(func(context.Context, string) (string, error) { return "workload-proof", nil }), ObserverFunc(func(Event) { panic("observer panic") }))
	err := client.WithLease(context.Background(), testRequest(), func(context.Context, *Lease) error { return nil })
	if err != nil || !revoked.Load() {
		t.Fatalf("error=%v revoked=%t; observer failure must not interrupt cleanup", err, revoked.Load())
	}
}

func testClient(t *testing.T, server *httptest.Server, identity IdentityTokenSource, observer Observer) *Client {
	t.Helper()
	tlsConfig := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	client, err := NewClient(Config{
		Endpoint:       server.URL,
		BrokerAudience: "credential-broker",
		Identity:       identity,
		BrokerAccess:   BootstrapCredentialFunc(func(context.Context) (string, error) { return "broker-access-key", nil }),
		TLSConfig:      tlsConfig,
		Observer:       observer,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func testRequest() Request {
	return Request{
		TaskID:       "task-42",
		Audience:     "catalog-api",
		Resource:     "/resource",
		TargetOrigin: "https://catalog-api.invalid",
		Actions:      []string{http.MethodGet},
		TTL:          2 * time.Minute,
		Auth:         APIKeyHeader("X-API-Key"),
	}
}

type ObserverFunc func(Event)

func (f ObserverFunc) ObserveCredentialLease(event Event) { f(event) }

type storedIdentity struct{ value string }

func (s storedIdentity) Token(context.Context, string) (string, error) { return s.value, nil }

type leaseResponseFixture struct {
	LeaseID    string    `json:"lease_id"`
	Credential string    `json:"credential"`
	ExpiresAt  time.Time `json:"expires_at"`
}
