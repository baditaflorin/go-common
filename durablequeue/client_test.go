package durablequeue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func TestClientLifecycleAndFencedReceipt(t *testing.T) {
	calls := 0
	transport := rt(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer ak_fixture" || r.URL.RawQuery != "" {
			t.Fatal("credential not bound to authorization header")
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/runs/run-1":
			return response(200, `{"created":true}`), nil
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			var b struct {
				Tasks []Task `json:"tasks"`
			}
			if json.NewDecoder(r.Body).Decode(&b) != nil || len(b.Tasks) != 1 {
				t.Fatal("wrong tasks")
			}
			return response(200, `{"accepted":1}`), nil
		case strings.HasSuffix(r.URL.Path, "/seal"):
			return response(200, `{"sealed":true}`), nil
		case strings.HasSuffix(r.URL.Path, "/claim"):
			return response(200, `{"key":"www.example.com","worker":"w1","lease_version":4,"attempts":2,"lease_until":"2030-01-01T00:00:00Z","payload":{"mode":"expanded"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/complete"):
			var v map[string]any
			if json.NewDecoder(r.Body).Decode(&v) != nil || v["lease_version"] != float64(4) || v["worker"] != "w1" {
				t.Fatal("lease fence missing")
			}
			return response(200, `{"completed":true}`), nil
		case r.Method == "GET":
			return response(200, `{"state":"running","sealed":true,"total":1,"pending":0,"leased":0,"done":1,"failed":0,"complete":true}`), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	c, err := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = c.CreateRun(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err = c.Enqueue(ctx, "run-1", []Task{{Key: "www.example.com", Payload: json.RawMessage(`{"mode":"expanded"}`), MaxAttempts: 3}}); err != nil {
		t.Fatal(err)
	}
	if err = c.Seal(ctx, "run-1", 1); err != nil {
		t.Fatal(err)
	}
	l, ok, err := c.Claim(ctx, "run-1", "w1", time.Minute)
	if err != nil || !ok || l.Version != 4 {
		t.Fatal(l, ok, err)
	}
	if err = c.Complete(ctx, "run-1", l, json.RawMessage(`{"capture_id":"fixture"}`)); err != nil {
		t.Fatal(err)
	}
	s, err := c.Stats(ctx, "run-1")
	if err != nil || !s.Complete || s.Done != 1 || calls != 6 {
		t.Fatal(s, calls, err)
	}
}
func TestNoWorkDoesNotImplyCompletionAndStaleLeaseFails(t *testing.T) {
	c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			return response(204, ""), nil
		}
		return response(409, "conflict"), nil
	})})
	_, ok, err := c.Claim(context.Background(), "run", "worker", time.Second)
	if err != nil || ok {
		t.Fatal(ok, err)
	}
	l := Lease{Key: "example.com", Worker: "worker", Version: 1}
	if err = c.Complete(context.Background(), "run", l, json.RawMessage(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
func TestRejectsInvalidOriginAndUnexpectedReceipts(t *testing.T) {
	for _, origin := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/path", "https://example.com?token=x"} {
		if _, e := New(Config{URL: origin}); !errors.Is(e, ErrInvalid) {
			t.Fatal("accepted", origin)
		}
	}
	for _, tc := range []struct {
		status int
		body   string
	}{{204, ""}, {200, `{"created":false}`}, {200, `{}`}, {302, ""}} {
		calls := 0
		c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(*http.Request) (*http.Response, error) {
			calls++
			r := response(tc.status, tc.body)
			if tc.status == 302 {
				r.Header.Set("Location", "https://other.example/")
			}
			return r, nil
		})})
		if err := c.CreateRun(context.Background(), "run"); !errors.Is(err, ErrUnavailable) || calls != 1 {
			t.Fatalf("unexpected receipt accepted: %v calls=%d", err, calls)
		}
	}
	c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(*http.Request) (*http.Response, error) {
		return response(200, `{"total":2,"pending":1,"done":0}`), nil
	})})
	if _, err := c.Stats(context.Background(), "run"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("inconsistent stats accepted", err)
	}
}
func TestEncodesTaskKeyAndPauseAck(t *testing.T) {
	c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/pause") {
			return response(200, `{"paused":true}`), nil
		}
		if !strings.Contains(r.URL.EscapedPath(), "a%2Fb") || r.URL.RawQuery != "" {
			t.Fatal("task key changed URL scope", r.URL.EscapedPath())
		}
		return response(200, `{"completed":true}`), nil
	})})
	if err := c.SetPaused(context.Background(), "run", true); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(context.Background(), "run", Lease{Key: "a/b", Worker: "w", Version: 1}, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}
func TestRejectsMismatchedClaimAndPreservesCancel(t *testing.T) {
	c, _ := New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(*http.Request) (*http.Response, error) {
		return response(200, `{"key":"example.com","worker":"other","lease_version":1,"attempts":1,"lease_until":"2030-01-01T00:00:00Z","payload":{}}`), nil
	})})
	if _, _, err := c.Claim(context.Background(), "run", "worker", time.Second); !errors.Is(err, ErrUnavailable) {
		t.Fatal("mismatched worker accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ = New(Config{URL: "https://queue.example", Token: "ak_fixture", Transport: rt(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })})
	if err := c.CreateRun(ctx, "run"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
}
