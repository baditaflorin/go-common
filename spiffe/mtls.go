package spiffe

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// X509Identity owns a rotating X.509-SVID source used to build mutually
// authenticated TLS configurations. Close it when the process shuts down.
type X509Identity struct {
	source *workloadapi.X509Source
}

// NewX509Identity connects to the local Unix Workload API and waits for the
// first SVID and trust bundle. It never accepts a remote TCP endpoint.
func NewX509Identity(ctx context.Context, socket string) (*X509Identity, error) {
	socket = strings.TrimSpace(socket)
	if ctx == nil || !validUnixSocket(socket) {
		return nil, ErrUnavailable
	}
	source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(socket)))
	if err != nil {
		return nil, ErrUnavailable
	}
	return &X509Identity{source: source}, nil
}

// ClientTLSConfig presents this workload's SVID and authorizes only the
// configured server identities.
func (i *X509Identity) ClientTLSConfig(allowedServerIDs []string) (*tls.Config, error) {
	ids, err := parseIDs(allowedServerIDs)
	if err != nil || i == nil || i.source == nil {
		return nil, ErrUnavailable
	}
	config := tlsconfig.MTLSClientConfig(i.source, i.source, tlsconfig.AuthorizeOneOf(ids...))
	config.MinVersion = tls.VersionTLS12
	return config, nil
}

// ServerTLSConfig presents this workload's SVID and requires clients to prove
// one of the exact configured SPIFFE identities.
func (i *X509Identity) ServerTLSConfig(allowedClientIDs []string) (*tls.Config, error) {
	ids, err := parseIDs(allowedClientIDs)
	if err != nil || i == nil || i.source == nil {
		return nil, ErrUnavailable
	}
	config := tlsconfig.MTLSServerConfig(i.source, i.source, tlsconfig.AuthorizeOneOf(ids...))
	config.MinVersion = tls.VersionTLS12
	return config, nil
}

func (i *X509Identity) Close() error {
	if i == nil || i.source == nil {
		return nil
	}
	return i.source.Close()
}

// HTTPClient contains a redirect-disabled HTTP client and the X.509 source it
// uses. The transport does not honor HTTP proxy environment variables because
// proxying would cross the configured SPIFFE peer-authentication boundary.
type HTTPClient struct {
	client    *http.Client
	identity  *X509Identity
	closeOnce sync.Once
	closeErr  error
}

func NewHTTPClient(ctx context.Context, socket string, allowedServerIDs []string) (*HTTPClient, error) {
	identity, err := NewX509Identity(ctx, socket)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := identity.ClientTLSConfig(allowedServerIDs)
	if err != nil {
		_ = identity.Close()
		return nil, err
	}
	return &HTTPClient{
		client: &http.Client{
			Transport: &http.Transport{
				Proxy: nil, TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 5 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 30 * time.Second,
				MaxIdleConns: 20, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 8,
			},
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		identity: identity,
	}, nil
}

func (c *HTTPClient) Do(req *http.Request) (*http.Response, error) {
	if c == nil || c.client == nil || req == nil {
		return nil, ErrUnavailable
	}
	return c.client.Do(req)
}

func (c *HTTPClient) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.CloseIdleConnections()
		c.closeErr = c.identity.Close()
	})
	return c.closeErr
}

func (c *HTTPClient) CloseIdleConnections() {
	if c == nil || c.client == nil {
		return
	}
	if transport, ok := c.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func parseIDs(rawIDs []string) ([]spiffeid.ID, error) {
	if len(rawIDs) == 0 || len(rawIDs) > 128 {
		return nil, errors.New("SPIFFE peer allowlist is required")
	}
	ids := make([]spiffeid.ID, 0, len(rawIDs))
	seen := make(map[string]struct{}, len(rawIDs))
	for _, rawID := range rawIDs {
		if strings.TrimSpace(rawID) != rawID {
			return nil, errors.New("invalid SPIFFE peer identity")
		}
		id, err := spiffeid.FromString(rawID)
		if err != nil || id.String() != rawID {
			return nil, errors.New("invalid SPIFFE peer identity")
		}
		if _, ok := seen[rawID]; ok {
			return nil, errors.New("duplicate SPIFFE peer identity")
		}
		seen[rawID] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}
