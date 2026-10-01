package broker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	credentiallease "github.com/baditaflorin/go-common/credentiallease"
)

func TestClientAndBrokerCompleteLeaseUseAndRevoke(t *testing.T) {
	receivedAPIKey := make(chan string, 1)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAPIKey <- r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("target-ok"))
	}))
	defer target.Close()

	issuedAt := time.Now().UTC()
	issuer := &testIssuer{now: issuedAt, value: testSecret}
	store := newTestStore()
	auditor := &testAuditor{}
	server, err := New(Config{
		Audience: "credential-broker",
		Verifier: testVerifier{identity: Identity{Issuer: "https://identity.example", Subject: "worker-1", WorkloadID: "go-app", TaskID: testTask}},
		Policy:   testPolicy{maxTTL: 3 * time.Minute},
		Store:    store,
		Auditor:  auditor,
		Issuers:  map[string]Issuer{"fake": issuer},
		Now:      func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	broker := httptest.NewTLSServer(server)
	defer broker.Close()
	roots := x509.NewCertPool()
	roots.AddCert(broker.Certificate())
	client, err := credentiallease.NewClient(credentiallease.Config{
		Endpoint:       broker.URL,
		BrokerAudience: "credential-broker",
		Identity: credentiallease.IdentityTokenSourceFunc(func(_ context.Context, audience string) (string, error) {
			if audience != "credential-broker" {
				t.Fatalf("identity requested for unexpected audience %q", audience)
			}
			return testProof, nil
		}),
		TLSConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		MaxLeaseTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	targetOrigin := "https://" + targetURL.Host
	err = client.WithLease(context.Background(), credentiallease.Request{
		TaskID:       testTask,
		Audience:     "catalog-api",
		Resource:     "catalog/items/42",
		TargetOrigin: targetOrigin,
		Actions:      []string{"read"},
		TTL:          2 * time.Minute,
		Auth:         credentiallease.APIKeyHeader("X-API-Key"),
	}, func(ctx context.Context, lease *credentiallease.Lease) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL+"/items/42", nil)
		if err != nil {
			return err
		}
		resp, err := lease.Do(target.Client(), req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("target status = %d", resp.StatusCode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithLease() error = %v", err)
	}
	if got := <-receivedAPIKey; got != testSecret {
		t.Fatalf("target API key = %q, want issued key", got)
	}
	issuer.mu.Lock()
	issues, revocations := issuer.issues, issuer.revocations
	issuer.mu.Unlock()
	if issues != 1 || revocations != 1 {
		t.Fatalf("provider lifecycle = issue:%d revoke:%d, want one each", issues, revocations)
	}
}
