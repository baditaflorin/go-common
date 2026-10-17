// Package main is a deliberately bad service. Every `want:` marker below is a
// finding that lint/selftest.sh requires the shared config to report; the
// functions without a marker must stay clean.
package main

import (
	"context"
	"io"
	"net/http"
	"time"

	"example.com/lintfixture/old"
)

func badGet() {
	resp, err := http.Get("https://example.com") // want: forbidigo
	if err == nil {
		_ = resp.Body.Close()
	}
}

func badDefaultClient(req *http.Request) {
	resp, err := http.DefaultClient.Do(req) // want: forbidigo
	if err == nil {
		_ = resp.Body.Close()
	}
}

func badListen() {
	_ = http.ListenAndServe(":8080", nil) // want: forbidigo
}

func badNoContext() *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil) // want: noctx
	return req
}

func badUnclosedBody(c *http.Client, req *http.Request) {
	resp, err := c.Do(req) // want: bodyclose
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
}

func badDeprecated() {
	old.Run() // want: SA1019
}

// goodClient is what the rules steer people to: its own client with a timeout,
// a context on the request, and the body closed.
func goodClient(ctx context.Context, url string) error {
	c := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func main() {
	badGet()
	badDefaultClient(nil)
	badListen()
	_ = badNoContext()
	badUnclosedBody(nil, nil)
	badDeprecated()
	_ = goodClient(context.Background(), "https://example.com")
}
