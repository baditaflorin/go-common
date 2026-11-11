package proxysupplier

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A canceled request can leave net/http's detached connection setup running.
// Check the real proxy tunnel closes after a stalled TLS handshake, rather than
// merely checking that Client.Do returns when its request is canceled.
func TestCanceledRequestClosesStalledProxyTLS(t *testing.T) {
	for name, makeClient := range map[string]func(Supplier, time.Duration) *http.Client{
		"legacy": HTTPClient, "fail_closed": HTTPClientFailClosed,
	} {
		t.Run(name, func(t *testing.T) {
			hello := make(chan struct{}, 1)
			closed := make(chan error, 1)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					http.Error(w, "expected CONNECT", http.StatusMethodNotAllowed)
					return
				}
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					closed <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					closed <- err
					return
				}
				if err := rw.Flush(); err != nil {
					closed <- err
					return
				}
				buf := make([]byte, 4096)
				if _, err := conn.Read(buf); err != nil {
					closed <- err
					return
				}
				hello <- struct{}{}
				_, err = io.Copy(io.Discard, conn)
				closed <- err
			}))
			defer proxy.Close()

			client := makeClient(&changingSupplier{address: proxy.URL}, 0)
			transport := client.Transport.(*graphObservedTransport).Transport
			if transport.DialContext == nil || transport.TLSHandshakeTimeout != 10*time.Second {
				t.Fatalf("unbounded connection setup: dial=%v TLS=%v", transport.DialContext != nil, transport.TLSHandshakeTimeout)
			}
			// Exercise the production transport with a shorter bound to keep the
			// test fast; the default value is asserted above.
			transport.TLSHandshakeTimeout = 250 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/", nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				resp, err := client.Do(req)
				if resp != nil {
					resp.Body.Close()
				}
				done <- err
			}()
			select {
			case <-hello:
			case <-time.After(2 * time.Second):
				t.Fatal("proxy did not receive TLS handshake")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled request unexpectedly succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("canceled request did not return")
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("proxy saw read failure, not EOF: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stalled proxy tunnel remained open")
			}
		})
	}
}
