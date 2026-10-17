package meshclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/circuitbreaker"
	"github.com/baditaflorin/go-common/clock"
	"github.com/baditaflorin/go-common/degraded"
	"github.com/baditaflorin/go-common/meshclient"
	"github.com/baditaflorin/go-common/resilience"
	"github.com/baditaflorin/go-common/retry"
	"github.com/baditaflorin/go-common/testhelpers/leakcheck"
)

type reply struct {
	Balance int64  `json:"balance"`
	Owner   string `json:"owner"`
}

func newClient(t *testing.T, srv *httptest.Server, mode meshclient.Mode, mutate func(*meshclient.Config)) *meshclient.Client {
	t.Helper()
	cfg := meshclient.Config{Name: "ledger", BaseURL: srv.URL, Mode: mode, Transport: srv.Client().Transport}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := meshclient.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func withSink(t *testing.T) (context.Context, *degraded.Sink) {
	t.Helper()
	s := degraded.New()
	return degraded.NewContext(context.Background(), s), s
}

func TestNewValidatesItsConfig(t *testing.T) {
	t.Parallel()
	ok := meshclient.Config{Name: "ledger", BaseURL: "http://ledger:8080", Mode: meshclient.FailClosed}
	if _, err := meshclient.New(ok); err != nil {
		t.Fatalf("a valid config was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*meshclient.Config){
		"no mode":            func(c *meshclient.Config) { c.Mode = 0 },
		"upper-case name":    func(c *meshclient.Config) { c.Name = "Ledger" },
		"empty name":         func(c *meshclient.Config) { c.Name = "" },
		"name with a space":  func(c *meshclient.Config) { c.Name = "my ledger" },
		"no scheme":          func(c *meshclient.Config) { c.BaseURL = "ledger:8080" },
		"ftp":                func(c *meshclient.Config) { c.BaseURL = "ftp://ledger" },
		"credentials in url": func(c *meshclient.Config) { c.BaseURL = "http://user:pass@ledger" },
		"query in url":       func(c *meshclient.Config) { c.BaseURL = "http://ledger?x=1" },
		"fragment in url":    func(c *meshclient.Config) { c.BaseURL = "http://ledger#x" },
		"no host":            func(c *meshclient.Config) { c.BaseURL = "http:///path" },
		"empty url":          func(c *meshclient.Config) { c.BaseURL = "" },
	} {
		cfg := ok
		mutate(&cfg)
		if _, err := meshclient.New(cfg); err == nil {
			t.Errorf("%s: New accepted %+v", name, cfg)
		}
	}
}

func TestGetDecodesJSONAndSendsTheStandardHeaders(t *testing.T) {
	leakcheck.Check(t)
	var gotPath, gotQuery, gotUA, gotAccept string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotUA, gotAccept = r.URL.Path, r.URL.RawQuery, r.UserAgent(), r.Header.Get("Accept")
		_, _ = io.WriteString(w, `{"balance":42,"owner":"ada","added_later":true}`)
	})
	c := newClient(t, srv, meshclient.FailClosed, func(cfg *meshclient.Config) {
		cfg.BaseURL = srv.URL + "/api" // a base path is kept
		cfg.UserAgent = "svc/1"
	})
	var out reply
	err := c.Do(context.Background(), meshclient.Request{Path: "/v1/balance", Query: map[string][]string{"acct": {"a b"}}}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Balance != 42 || out.Owner != "ada" {
		t.Fatalf("decoded %+v", out)
	}
	if gotPath != "/api/v1/balance" || gotQuery != "acct=a+b" || gotUA != "svc/1" || gotAccept != "application/json" {
		t.Fatalf("path=%q query=%q ua=%q accept=%q", gotPath, gotQuery, gotUA, gotAccept)
	}
}

func TestPostJSONSendsTheBodyAndContentType(t *testing.T) {
	t.Parallel()
	var gotBody, gotType, gotMethod string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotType, gotMethod = string(b), r.Header.Get("Content-Type"), r.Method
		_, _ = io.WriteString(w, `{"balance":1}`)
	})
	c := newClient(t, srv, meshclient.FailClosed, nil)
	var out reply
	if err := c.PostJSON(context.Background(), "/v1/charge", map[string]any{"amount": 5}, &out); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotType != "application/json" || gotBody != `{"amount":5}` {
		t.Fatalf("method=%q type=%q body=%q", gotMethod, gotType, gotBody)
	}
}

func TestAClientErrorIsAStatusErrorNotUnavailable(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"no such account"}`)
	})
	c := newClient(t, srv, meshclient.FailOpen, nil)
	ctx, sink := withSink(t)
	err := c.Get(ctx, "/v1/account/9", nil)

	var se *meshclient.StatusError
	if !errors.As(err, &se) || se.Status != 404 || !strings.Contains(string(se.Body), "no such account") {
		t.Fatalf("err = %v, want a *StatusError 404 carrying the body", err)
	}
	if meshclient.IsUnavailable(err) || resilience.Retryable(err) {
		t.Fatal("a 404 means the sibling answered; it is neither unavailable nor retryable")
	}
	if got := sink.Slice(); len(got) != 0 {
		t.Fatalf("degraded = %v; a 404 must not be reported as degradation", got)
	}
	if strings.Contains(err.Error(), "no such account") {
		t.Fatal("the reply body leaked into the error text")
	}
}

func TestUnavailabilityClassesAndTheDegradedTag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		timeout time.Duration
		class   string
	}{
		{"503", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, 0, "down"},
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }, 0, "down"},
		{"504", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(504) }, 0, "timeout"},
		{"429", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }, 0, "rate_limited"},
		{"slow", func(w http.ResponseWriter, _ *http.Request) { time.Sleep(400 * time.Millisecond) }, 60 * time.Millisecond, "timeout"},
		{"garbage", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `<html>`) }, 0, "down"},
		{"empty", func(w http.ResponseWriter, _ *http.Request) {}, 0, "down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := serve(t, tc.handler)
			for _, mode := range []meshclient.Mode{meshclient.FailOpen, meshclient.FailClosed} {
				c := newClient(t, srv, mode, func(cfg *meshclient.Config) { cfg.Timeout = tc.timeout })
				ctx, sink := withSink(t)
				var out reply
				err := c.Get(ctx, "/x", &out)

				var ue *meshclient.UnavailableError
				if !errors.As(err, &ue) || !meshclient.IsUnavailable(err) || ue.Class != tc.class {
					t.Fatalf("%s: err = %v, want an UnavailableError of class %q", mode, err, tc.class)
				}
				tags := sink.Slice()
				if mode == meshclient.FailOpen {
					if len(tags) != 1 || tags[0] != "ledger-"+tc.class {
						t.Fatalf("fail-open degraded = %v, want [ledger-%s]", tags, tc.class)
					}
				} else if len(tags) != 0 {
					t.Fatalf("fail-closed recorded degradation %v; the request fails instead", tags)
				}
			}
		})
	}
}

func TestAConnectionThatCannotBeMadeIsDown(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	c := newClient(t, srv, meshclient.FailOpen, nil)
	srv.Close() // nothing is listening any more
	ctx, sink := withSink(t)
	err := c.Get(ctx, "/x", nil)
	if !meshclient.IsUnavailable(err) || len(sink.Slice()) != 1 || sink.Slice()[0] != "ledger-down" {
		t.Fatalf("err=%v degraded=%v, want unavailable and ledger-down", err, sink.Slice())
	}
}

func TestACallerThatGivesUpIsNotTheSiblingsFault(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { time.Sleep(300 * time.Millisecond) })
	c := newClient(t, srv, meshclient.FailOpen, nil)
	ctx, sink := withSink(t)
	ctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	err := c.Get(ctx, "/x", nil)
	if err == nil || meshclient.IsUnavailable(err) || len(sink.Slice()) != 0 {
		t.Fatalf("err=%v degraded=%v: a cancelled call is not unavailability", err, sink.Slice())
	}
}

func TestResponseLimitsAndStrictDecoding(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			_, _ = io.WriteString(w, `{"owner":"`+strings.Repeat("a", 5000)+`"}`)
		case "/trailing":
			_, _ = io.WriteString(w, `{"balance":1} {"balance":2}`)
		default:
			_, _ = io.WriteString(w, `{"balance":1,"surprise":true}`)
		}
	})
	c := newClient(t, srv, meshclient.FailClosed, func(cfg *meshclient.Config) { cfg.MaxBodyBytes = 1024 })
	var out reply
	if err := c.Get(context.Background(), "/big", &out); !meshclient.IsUnavailable(err) {
		t.Fatalf("an over-size reply: %v, want unavailable", err)
	}
	if err := c.Get(context.Background(), "/trailing", &out); !meshclient.IsUnavailable(err) {
		t.Fatalf("data after the JSON value: %v, want unavailable", err)
	}
	if err := c.Get(context.Background(), "/ok", &out); err != nil {
		t.Fatalf("an unknown field must be tolerated by default: %v", err)
	}
	strict := newClient(t, srv, meshclient.FailClosed, func(cfg *meshclient.Config) { cfg.RejectUnknownFields = true })
	if err := strict.Get(context.Background(), "/ok", &out); !meshclient.IsUnavailable(err) {
		t.Fatalf("RejectUnknownFields: %v, want the unknown field rejected", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	c := newClient(t, srv, meshclient.FailClosed, nil)
	var se *meshclient.StatusError
	if err := c.Get(context.Background(), "/x", nil); !errors.As(err, &se) || se.Status != 302 || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d, want a StatusError 302 after one request", err, hits.Load())
	}
}

func TestRequestAndConfigProblemsAreNotUnavailability(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(http.ResponseWriter, *http.Request) {})
	c := newClient(t, srv, meshclient.FailOpen, nil)
	ctx, sink := withSink(t)
	if err := c.Get(ctx, "no-slash", nil); err == nil || meshclient.IsUnavailable(err) {
		t.Fatalf("a path without '/': %v", err)
	}
	if err := c.PostJSON(ctx, "/x", make(chan int), nil); err == nil || meshclient.IsUnavailable(err) {
		t.Fatalf("an unmarshalable body: %v", err)
	}
	if len(sink.Slice()) != 0 {
		t.Fatalf("programmer errors were reported as degradation: %v", sink.Slice())
	}
}

// With a policy the client retries what is worth retrying, replays the body each
// time, and still reports the degradation once per call.
func TestPolicyRetriesReplaysTheBodyAndTagsOnce(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	var bodies []string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"balance":7}`)
	})
	p := resilience.New("ledger", resilience.WithBackoff(retry.NoBackoff()))
	c := newClient(t, srv, meshclient.FailOpen, func(cfg *meshclient.Config) { cfg.Policy = p })
	ctx, sink := withSink(t)
	var out reply
	if err := c.PostJSON(ctx, "/v1/charge", map[string]int{"amount": 9}, &out); err != nil || out.Balance != 7 {
		t.Fatalf("err=%v out=%+v, want success on the third attempt", err, out)
	}
	for i, b := range bodies {
		if b != `{"amount":9}` {
			t.Fatalf("attempt %d sent body %q; every retry must replay the body", i+1, b)
		}
	}
	if len(sink.Slice()) != 0 {
		t.Fatalf("a call that succeeded after retries recorded %v", sink.Slice())
	}

	// A call that never succeeds is reported once, however many attempts it took.
	attempts.Store(-100)
	ctx, sink = withSink(t)
	if err := c.Get(ctx, "/v1/balance", &out); !meshclient.IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if got := sink.Slice(); len(got) != 1 || got[0] != "ledger-down" {
		t.Fatalf("degraded = %v, want exactly one ledger-down", got)
	}
}

func TestPolicyBudgetStopsRetriesAndBreakerRefuses(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	clk := clock.NewMock(time.Unix(0, 0))
	budget := resilience.NewBudget(0, 0, resilience.WithBudgetClock(clk), resilience.WithBurst(2))
	br := circuitbreaker.New(circuitbreaker.Options{Upstream: "ledger", FailureThreshold: 4, OpenFor: time.Hour})
	p := resilience.New("ledger", resilience.WithBackoff(retry.NoBackoff()), resilience.WithBudget(budget), resilience.WithBreaker(br))
	c := newClient(t, srv, meshclient.FailClosed, func(cfg *meshclient.Config) { cfg.Policy = p })

	for i := 0; i < 3; i++ {
		_ = c.Get(context.Background(), "/x", nil)
	}
	// 3 calls: 3 first attempts + the 2 retries the budget allows = 5, but the
	// breaker opens at 4 failures, so the fifth attempt is refused.
	if got := attempts.Load(); got != 4 {
		t.Fatalf("upstream saw %d requests, want 4 (budget of 2 retries; breaker opens at 4 failures)", got)
	}
	before := attempts.Load()
	err := c.Get(context.Background(), "/x", nil)
	if !meshclient.IsUnavailable(err) || !errors.Is(err, resilience.ErrCircuitOpen) || attempts.Load() != before {
		t.Fatalf("err=%v new requests=%d, want a refusal from the open breaker", err, attempts.Load()-before)
	}
}

func TestAnUnrecognisedErrorFromThePolicyIsNotUnavailability(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(http.ResponseWriter, *http.Request) {})
	c := newClient(t, srv, meshclient.FailOpen, nil)
	ctx, sink := withSink(t)
	var raw json.RawMessage
	// An empty 200 into a RawMessage is a decode problem, which is unavailability.
	if err := c.Get(ctx, "/x", &raw); !meshclient.IsUnavailable(err) {
		t.Fatalf("err = %v", err)
	}
	if len(sink.Slice()) != 1 {
		t.Fatalf("degraded = %v", sink.Slice())
	}
}
