package credentiallease

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func streamTestLease(target string, ctx context.Context) *Lease {
	return &Lease{id: "test", expiresAt: time.Now().Add(time.Minute), placement: BearerAuth(), targetOrigin: target, resource: "/", actions: []string{http.MethodGet}, ctx: ctx, state: &leaseState{credential: []byte("test-token")}}
}

func TestLeaseDoKeepsContextAliveUntilStreamingBodyFinishes(t *testing.T) {
	bodyReady := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-bodyReady:
			_, _ = io.WriteString(w, "delayed response")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	lease := streamTestLease(server.URL, context.Background())
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/", nil)
	resp, err := lease.Do(server.Client(), req)
	if err != nil {
		close(bodyReady)
		t.Fatal(err)
	}
	defer resp.Body.Close()
	close(bodyReady)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "delayed response" {
		t.Fatalf("stream body=%q err=%v", body, err)
	}
}

type leaseBodyTransport struct{ request *http.Request }

func (rt *leaseBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.request = req
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("body")), Request: req}, nil
}

func TestLeaseBodyCleansUpOnEOFAndClose(t *testing.T) {
	for _, eof := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "eof"}[eof], func(t *testing.T) {
			transport := &leaseBodyTransport{}
			lease := streamTestLease("https://example.test", context.Background())
			req, _ := http.NewRequest(http.MethodGet, "https://example.test/", nil)
			resp, err := lease.Do(&http.Client{Transport: transport}, req)
			if err != nil {
				t.Fatal(err)
			}
			if err := transport.request.Context().Err(); err != nil {
				t.Fatalf("request canceled before body read: %v", err)
			}
			if eof {
				_, err = io.ReadAll(resp.Body)
			} else {
				err = resp.Body.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if transport.request.Context().Err() == nil {
				t.Fatal("body completion did not release context")
			}
			if got := transport.request.Header.Get("Authorization"); got != "" {
				t.Fatal("credential retained after body completion")
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLeaseBodyStillHonorsRequestCancellation(t *testing.T) {
	transport := &leaseBodyTransport{}
	lease := streamTestLease("https://example.test", context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/", nil)
	resp, err := lease.Do(&http.Client{Transport: transport}, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	select {
	case <-transport.request.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("request cancellation was not propagated")
	}
}
