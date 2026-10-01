package credentiallease

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

const taskTestProof = "header.payload.signature"

func taskTestServer(t *testing.T, acquireCount *atomic.Int32, closeCounts ...*atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-API-Key") != "bootstrap-service-key" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(struct {
			TaskID    string    `json:"task_id"`
			Proof     string    `json:"proof"`
			ExpiresAt time.Time `json:"expires_at"`
		}{"task-123", taskTestProof, time.Now().Add(5 * time.Minute).UTC()})
	})
	mux.HandleFunc("/v1/leases", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+taskTestProof || r.Header.Get("X-API-Key") != "bootstrap-service-key" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		acquireCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(struct {
			LeaseID    string    `json:"lease_id"`
			Credential string    `json:"credential"`
			ExpiresAt  time.Time `json:"expires_at"`
		}{"lease-1", "secret-value", time.Now().Add(30 * time.Second).UTC()})
	})
	mux.HandleFunc("/v1/leases/lease-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "Bearer "+taskTestProof || r.Header.Get("X-API-Key") != "bootstrap-service-key" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/tasks/task-123", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "Bearer "+taskTestProof || r.Header.Get("X-API-Key") != "bootstrap-service-key" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		if len(closeCounts) > 0 && closeCounts[0] != nil {
			closeCounts[0].Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestTaskManagerWithTaskClosesAfterCallbackError(t *testing.T) {
	var acquired, closed atomic.Int32
	server := taskTestServer(t, &acquired, &closed)
	manager := newTaskTestManager(t, server)
	wantErr := errors.New("callback failed")
	err := manager.WithTask(context.Background(), func(_ context.Context, task *Task) error {
		if task.ID() != "task-123" {
			t.Fatalf("unexpected task id %q", task.ID())
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("callback error was not preserved: %v", err)
	}
	if closed.Load() != 1 {
		t.Fatalf("task close count=%d, want 1", closed.Load())
	}
}

func TestTaskManagerWithTaskClosesDuringPanic(t *testing.T) {
	var acquired, closed atomic.Int32
	server := taskTestServer(t, &acquired, &closed)
	manager := newTaskTestManager(t, server)
	panicked := false
	func() {
		defer func() {
			panicked = recover() == "callback panic"
		}()
		_ = manager.WithTask(context.Background(), func(context.Context, *Task) error {
			panic("callback panic")
		})
	}()
	if !panicked {
		t.Fatal("callback panic did not propagate")
	}
	if closed.Load() != 1 {
		t.Fatalf("task close count=%d, want 1 after panic", closed.Load())
	}
}

func TestTaskCloseCancelsInFlightLeaseCallback(t *testing.T) {
	var acquired atomic.Int32
	server := taskTestServer(t, &acquired)
	manager := newTaskTestManager(t, server)
	task, err := manager.BeginTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	client, err := task.Client(Config{})
	if err != nil {
		t.Fatal(err)
	}
	callbackDone := make(chan struct{})
	leaseErr := make(chan error, 1)
	go func() {
		leaseErr <- client.WithLease(context.Background(), Request{
			TaskID: task.ID(), Audience: "catalog-api", Resource: "/record/7",
			TargetOrigin: "https://target.invalid", Actions: []string{http.MethodGet},
			TTL: time.Minute, Auth: APIKeyHeader("X-API-Key"),
		}, func(ctx context.Context, _ *Lease) error {
			close(callbackDone)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-callbackDone:
	case <-time.After(2 * time.Second):
		t.Fatal("lease callback did not start")
	}
	if err := task.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-leaseErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected callback cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task close did not cancel the lease callback")
	}
	if acquired.Load() != 1 {
		t.Fatalf("expected one lease request, got %d", acquired.Load())
	}
}

func newTaskTestManager(t *testing.T, server *httptest.Server) *TaskManager {
	t.Helper()
	cert := server.Certificate()
	endpoint := server.URL
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	manager, err := NewTaskManager(TaskManagerConfig{
		Endpoint: endpoint, BrokerAudience: "https://broker.invalid",
		Bootstrap: BootstrapCredentialFunc(func(context.Context) (string, error) { return "bootstrap-service-key", nil }),
		TLSConfig: &tls.Config{RootCAs: pool}, TaskTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestTaskManagerCreatesTaskAndTaskClientRevokesLease(t *testing.T) {
	var acquired atomic.Int32
	server := taskTestServer(t, &acquired)
	manager := newTaskTestManager(t, server)

	task, err := manager.BeginTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if task.ID() != "task-123" || task.ExpiresAt().IsZero() {
		t.Fatalf("unexpected task metadata: %s %s", task.ID(), task.ExpiresAt())
	}
	if got := fmt.Sprintf("%#v %s %+v", task, task, task); got == "" || strings.Contains(got, taskTestProof) {
		t.Fatalf("task proof leaked through formatting: %s", got)
	}
	if raw, err := json.Marshal(task); err != nil || strings.Contains(string(raw), taskTestProof) {
		t.Fatalf("task proof leaked through JSON: %s (%v)", raw, err)
	}

	client, err := task.Client(Config{MaxLeaseTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	err = client.WithLease(context.Background(), Request{
		TaskID: task.ID(), Audience: "catalog-api", Resource: "/record/7",
		TargetOrigin: "https://target.invalid", Actions: []string{http.MethodGet},
		TTL: time.Minute, Auth: APIKeyHeader("X-API-Key"),
	}, func(_ context.Context, lease *Lease) error {
		if strings.Contains(fmt.Sprintf("%#v %s %+v", lease, lease, lease), "secret-value") {
			t.Fatal("lease credential leaked through formatting")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if acquired.Load() != 1 {
		t.Fatalf("expected one lease acquisition, got %d", acquired.Load())
	}
	if err := task.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := task.Close(context.Background()); err != nil {
		t.Fatalf("repeated close should be idempotent: %v", err)
	}
}

func TestTaskClientRejectsCallerChosenTaskID(t *testing.T) {
	var acquired atomic.Int32
	server := taskTestServer(t, &acquired)
	manager := newTaskTestManager(t, server)
	task, err := manager.BeginTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	client, err := task.Client(Config{})
	if err != nil {
		t.Fatal(err)
	}
	err = client.WithLease(context.Background(), Request{
		TaskID: "invented-task", Audience: "catalog-api", Resource: "/record/7",
		TargetOrigin: "https://target.invalid", Actions: []string{http.MethodGet},
		TTL: time.Minute, Auth: APIKeyHeader("X-API-Key"),
	}, func(context.Context, *Lease) error { return nil })
	if !errors.Is(err, ErrTaskMismatch) {
		t.Fatalf("expected task mismatch, got %v", err)
	}
	if acquired.Load() != 0 {
		t.Fatal("mismatched task request reached lease endpoint")
	}
}

func TestTaskManagerBlocksBootstrapRedirect(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			redirected.Store(true)
		}
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/tasks", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	certPool := x509.NewCertPool()
	certPool.AddCert(source.Certificate())
	manager, err := NewTaskManager(TaskManagerConfig{
		Endpoint: source.URL, BrokerAudience: "https://broker.invalid",
		Bootstrap: BootstrapCredentialFunc(func(context.Context) (string, error) { return "bootstrap-service-key", nil }),
		TLSConfig: &tls.Config{RootCAs: certPool},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.BeginTask(context.Background())
	if !errors.Is(err, ErrTaskCreateFailed) {
		t.Fatalf("expected task creation failure, got %v", err)
	}
	if redirected.Load() {
		t.Fatal("bootstrap credential followed a redirect")
	}
}

func TestTaskManagerClosesTaskAfterMalformedCreateResponse(t *testing.T) {
	var closed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id": "task-123", "proof": taskTestProof,
			"expires_at": time.Now().Add(10 * time.Minute).UTC(),
		})
	})
	mux.HandleFunc("/v1/tasks/task-123", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "Bearer "+taskTestProof || r.Header.Get("X-API-Key") != "bootstrap-service-key" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		closed.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	manager := newTaskTestManager(t, server)
	if _, err := manager.BeginTask(context.Background()); !errors.Is(err, ErrTaskCreateFailed) {
		t.Fatalf("expected invalid task TTL response to fail, got %v", err)
	}
	if !closed.Load() {
		t.Fatal("broker-created task was not closed after an unusable create response")
	}
}
