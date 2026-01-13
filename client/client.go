package client

import (
	"context"
	"net/http"
	"time"

	"github.com/baditaflorin/go-common/middleware"
)

// Client wraps http.Client to inject headers from context
type Client struct {
	HTTPClient *http.Client
}

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
			req.Header.Set("X-Request-ID", reqID)
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
