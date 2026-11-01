//go:build !gocommon_nospiffe

package server

import (
	"context"
	"time"

	"github.com/baditaflorin/go-common/secrets"
	"github.com/baditaflorin/go-common/spiffe"
)

// secretsSPIFFEClient builds the vault read path's SPIFFE mTLS client on package spiffe. A binary built with
// -tags gocommon_nospiffe does not link package spiffe: see secretsmtls_nospiffe.go.
func secretsSPIFFEClient(ctx context.Context, socket string, allowedServerIDs []string, timeout time.Duration) (secrets.SPIFFEClient, error) {
	client, err := spiffe.NewHTTPClientWithTimeout(ctx, socket, allowedServerIDs, timeout)
	if err != nil {
		return nil, err
	}
	return client, nil
}
