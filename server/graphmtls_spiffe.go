//go:build !gocommon_nospiffe

package server

import (
	"context"
	"time"

	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/spiffe"
)

// graphMTLSClient builds graph's SPIFFE mTLS client on package spiffe. A binary built
// with -tags gocommon_nospiffe does not link package spiffe (go-spiffe, go-jose and,
// unless something else brings it, gRPC): see graphmtls_nospiffe.go.
func graphMTLSClient(ctx context.Context, socket string, allowedServerIDs []string, timeout time.Duration) (graph.MTLSClient, error) {
	client, err := spiffe.NewHTTPClientWithTimeout(ctx, socket, allowedServerIDs, timeout)
	if err != nil {
		return nil, err
	}
	return client, nil
}
