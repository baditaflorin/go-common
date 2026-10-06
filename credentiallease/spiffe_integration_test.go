//go:build spiffe_integration

package credentiallease

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/spiffe"
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
