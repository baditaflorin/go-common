//go:build spiffe_integration

package credentiallease

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/spiffe"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// TestSPIFFETaskLifecycle talks to a live staging broker using a Workload API
// socket. It proves the SPIFFE-only path can create and close a task without a
// bootstrap API key. Run only from an enrolled canary container.
func TestSPIFFETaskLifecycle(t *testing.T) {
	socket := os.Getenv("SPIFFE_ENDPOINT_SOCKET")
	endpoint := os.Getenv("SPIFFE_BROKER_ENDPOINT")
	serverID := os.Getenv("SPIFFE_BROKER_SERVER_ID")
	audience := os.Getenv("SPIFFE_BROKER_AUDIENCE")
	if socket == "" || endpoint == "" || serverID == "" || audience == "" {
		t.Skip("SPIFFE staging endpoint is not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := spiffe.NewHTTPClient(ctx, socket, []string{serverID})
	if err != nil {
		t.Fatalf("connect to SPIFFE Workload API: %v", err)
	}
	manager, err := NewTaskManager(TaskManagerConfig{
		Endpoint:       endpoint,
		BrokerAudience: audience,
		SPIFFEClient:   client,
		RequestTimeout: 10 * time.Second,
	})
	if err != nil {
		_ = client.Close()
		t.Fatalf("configure SPIFFE-only task manager: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close SPIFFE task manager: %v", err)
		}
	})

	task, err := manager.BeginTask(ctx)
	if err != nil {
		t.Fatalf("create task with SPIFFE mTLS and no bootstrap key: %v", err)
	}
	if task == nil || task.ID() == "" {
		t.Fatal("broker returned an empty task identity")
	}
	if err := task.Close(ctx); err != nil {
		t.Fatalf("close task using the same SPIFFE peer: %v", err)
	}

	deniedTask, err := manager.BeginTask(ctx)
	if err != nil {
		t.Fatalf("create task for default-deny lease check: %v", err)
	}
	leaseClient, err := deniedTask.Client(Config{MaxLeaseTTL: 30 * time.Second, RequestTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("configure staging lease client: %v", err)
	}
	leaseErr := leaseClient.WithLease(ctx, Request{
		TaskID:       deniedTask.ID(),
		Audience:     "staging-default-deny",
		Resource:     "/health",
		TargetOrigin: "https://synthetic.invalid",
		Actions:      []string{http.MethodGet},
		TTL:          10 * time.Second,
		Auth:         APIKeyHeader("X-API-Key"),
	}, func(context.Context, *Lease) error {
		return errors.New("default-deny staging unexpectedly issued a lease")
	})
	if !errors.Is(leaseErr, ErrBrokerRejected) {
		t.Fatalf("staging policy did not reject an unconfigured lease: %v", leaseErr)
	}
	if err := deniedTask.Close(ctx); err != nil {
		t.Fatalf("close task after default-deny lease check: %v", err)
	}

	wrongServerClient, err := spiffe.NewHTTPClient(ctx, socket, []string{serverID + "-untrusted"})
	if err != nil {
		t.Fatalf("configure negative server-identity check: %v", err)
	}
	wrongServerManager, err := NewTaskManager(TaskManagerConfig{
		Endpoint:       endpoint,
		BrokerAudience: audience,
		SPIFFEClient:   wrongServerClient,
		RequestTimeout: 10 * time.Second,
	})
	if err != nil {
		_ = wrongServerClient.Close()
		t.Fatalf("configure negative SPIFFE-only task manager: %v", err)
	}
	defer wrongServerManager.Close()
	if _, err := wrongServerManager.BeginTask(ctx); err == nil {
		t.Fatal("broker with an unapproved SPIFFE server identity was accepted")
	}
}

// TestSPIFFESVIDRotation runs against the short-TTL isolated staging trust
// domain. It watches one long-lived Workload API source until the SVID serial
// changes, then proves the rotated client identity can still use the broker.
func TestSPIFFESVIDRotation(t *testing.T) {
	socket := os.Getenv("SPIFFE_ENDPOINT_SOCKET")
	endpoint := os.Getenv("SPIFFE_BROKER_ENDPOINT")
	serverID := os.Getenv("SPIFFE_BROKER_SERVER_ID")
	audience := os.Getenv("SPIFFE_BROKER_AUDIENCE")
	if socket == "" || endpoint == "" || serverID == "" || audience == "" {
		t.Skip("SPIFFE staging endpoint is not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(socket)))
	if err != nil {
		t.Fatalf("connect to SPIFFE Workload API: %v", err)
	}
	defer source.Close()

	initial, err := source.GetX509SVID()
	if err != nil || len(initial.Certificates) == 0 {
		t.Fatalf("read initial staging X509-SVID: err=%v", err)
	}
	initialSerial := initial.Certificates[0].SerialNumber.String()
	deadline := time.NewTimer(time.Until(initial.Certificates[0].NotAfter) + 30*time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for the staging X509-SVID to rotate")
		case <-deadline.C:
			t.Fatal("staging X509-SVID did not rotate before its expiry")
		case <-ticker.C:
			current, err := source.GetX509SVID()
			if err != nil || len(current.Certificates) == 0 {
				t.Fatalf("read current staging X509-SVID: err=%v", err)
			}
			if current.Certificates[0].SerialNumber.String() == initialSerial {
				continue
			}
			if !current.Certificates[0].NotAfter.After(time.Now()) {
				t.Fatal("rotated staging X509-SVID is already expired")
			}
			goto rotated
		}
	}

rotated:
	client, err := spiffe.NewHTTPClient(ctx, socket, []string{serverID})
	if err != nil {
		t.Fatalf("configure client after SVID rotation: %v", err)
	}
	manager, err := NewTaskManager(TaskManagerConfig{
		Endpoint: endpoint, BrokerAudience: audience, SPIFFEClient: client,
		RequestTimeout: 10 * time.Second,
	})
	if err != nil {
		_ = client.Close()
		t.Fatalf("configure broker client after SVID rotation: %v", err)
	}
	defer manager.Close()
	task, err := manager.BeginTask(ctx)
	if err != nil {
		t.Fatalf("broker rejected the rotated staging SVID: %v", err)
	}
	if err := task.Close(ctx); err != nil {
		t.Fatalf("close task after staging SVID rotation: %v", err)
	}
}
