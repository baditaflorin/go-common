package client

import (
	"context"
	"net/http"
	"time"

	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/middleware"
)

// Client wraps http.Client to inject headers from context
//
// Deprecated: use safehttp.NewClient for calls to the public internet, or
// meshclient for calls to a sibling fleet service; both propagate the W3C trace
// context by default. Client only copies the X-Request-ID header from the
// request context (see middleware.GetRequestID); a caller that needs that can
// set header.RequestID itself. The rest of this package (Fetch, JSProxy,
// Capability, the network-extraction helpers) is not deprecated and has no
// replacement. Removal no earlier than v0.104.0.
type Client struct {
	HTTPClient *http.Client
}

// New returns a Client with a 30 second timeout.
//
// Deprecated: use safehttp.NewClient (public internet) or meshclient (sibling
// fleet services); see Client. Removal no earlier than v0.104.0.
func New() *Client {
	return &Client{
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// Propagate X-Request-ID
	if ctx := req.Context(); ctx != nil {
		reqID := middleware.GetRequestID(ctx)
		if reqID != "" {
			req.Header.Set(header.RequestID, reqID)
		}
	}
	return c.HTTPClient.Do(req)
}

// Get helper
func (c *Client) Get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Post helper
//
// Deprecated: Post never sends its body argument (the request goes out with an
// empty body), so it cannot be used for a real POST. Build the request with
// http.NewRequestWithContext and send it with safehttp.NewClient (public
// internet) or meshclient (sibling fleet services). Removal no earlier than
// v0.104.0.
func (c *Client) Post(ctx context.Context, url string, contentType string, body interface{}) (*http.Response, error) {
	// Implementation omitted for brevity to keep it simple for now,
	// typically needs standard body handling.
	// For Phase 3, context propagation is the key.
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil) // Body handling needed
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return c.Do(req)
}
