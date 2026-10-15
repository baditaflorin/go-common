package apikey

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/header"
)

// node is a fake keystore node: it answers /verify with a fixed status after an
// optional delay and counts the requests it saw.
type node struct {
	srv    *httptest.Server
	hits   atomic.Int64
	status atomic.Int64
	delay  time.Duration
	user   string
}

func newNode(t *testing.T, status int, delay time.Duration, user string) *node {
	t.Helper()
	n := &node{delay: delay, user: user}
	n.status.Store(int64(status))
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.hits.Add(1)
		if n.delay > 0 {
			select {
			case <-time.After(n.delay):
			case <-r.Context().Done():
				return
			}
		}
		if n.status.Load() == http.StatusOK {
			w.Header().Set(header.AuthUser, n.user)
		}
		w.WriteHeader(int(n.status.Load()))
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// deadURL is an address nothing listens on (connection refused).
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

type recordObs struct {
	mu     sync.Mutex
	events []VerifyEvent
}

func (r *recordObs) ObserveVerify(e VerifyEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordObs) last() VerifyEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

func meshClient(eps ...string) *Client {
	return &Client{
		BaseURL:    eps[0],
		Endpoints:  eps,
		HedgeDelay: 50 * time.Millisecond,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		UserAgent:  "test",
	}
}

func TestVerify_SingleEndpointUnchanged(t *testing.T) {
	resetBreakers()
	for _, tc := range []struct {
		status int
		want   error
	}{{200, nil}, {401, ErrInvalidKey}, {503, ErrKeystoreUnavailable}} {
		n := newNode(t, tc.status, 0, "alice")
		c := &Client{BaseURL: n.srv.URL, HTTPClient: n.srv.Client(), UserAgent: "test"}
		res, err := c.Verify(context.Background(), "ak_x")
		if tc.want == nil {
			if err != nil || res.User != "alice" {
				t.Fatalf("status %d: %+v %v", tc.status, res, err)
			}
		} else if !errors.Is(err, tc.want) {
			t.Fatalf("status %d: got %v, want %v", tc.status, err, tc.want)
		}
		if n.hits.Load() != 1 {
			t.Fatalf("status %d: %d requests, want exactly 1", tc.status, n.hits.Load())
		}
	}
}

func TestVerify_DeadLocalNodeFailsOverAtOnce(t *testing.T) {
	resetBreakers()
	peer := newNode(t, 200, 0, "bob")
	c := meshClient(deadURL(t), peer.srv.URL)
	c.HedgeDelay = 5 * time.Second // failover must not wait for the hedge on a refused connection
	start := time.Now()
	res, err := c.Verify(context.Background(), "ak_x")
	if err != nil || res.User != "bob" {
		t.Fatalf("%+v %v", res, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("failover took %v; a refused connection must move on immediately", d)
	}
}

func TestVerify_SlowNodeIsHedged(t *testing.T) {
	resetBreakers()
	slow := newNode(t, 200, 2*time.Second, "slow")
	fast := newNode(t, 200, 0, "fast")
	obs := &recordObs{}
	c := meshClient(slow.srv.URL, fast.srv.URL)
	c.VerifyObs = obs
	start := time.Now()
	res, err := c.Verify(context.Background(), "ak_x")
	if err != nil || res.User != "fast" {
		t.Fatalf("%+v %v", res, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("hedged answer took %v", d)
	}
	ev := obs.last()
	if !ev.Hedged || ev.Attempts != 2 || ev.Endpoint != fast.srv.URL || ev.Result != "ok" {
		t.Fatalf("event %+v", ev)
	}
}

func TestVerify_401IsDefinitiveAndNotRetried(t *testing.T) {
	resetBreakers()
	local := newNode(t, 401, 0, "")
	peer := newNode(t, 200, 0, "should-not-be-asked")
	c := meshClient(local.srv.URL, peer.srv.URL)
	if _, err := c.Verify(context.Background(), "ak_bad"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("got %v", err)
	}
	time.Sleep(100 * time.Millisecond) // past the hedge delay
	if peer.hits.Load() != 0 {
		t.Fatalf("a 401 must not be re-asked elsewhere (peer saw %d requests)", peer.hits.Load())
	}
}

func TestVerify_5xxMovesOn(t *testing.T) {
	resetBreakers()
	sick := newNode(t, 503, 0, "")
	peer := newNode(t, 200, 0, "bob")
	c := meshClient(sick.srv.URL, peer.srv.URL)
	if res, err := c.Verify(context.Background(), "ak_x"); err != nil || res.User != "bob" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestVerify_AllDownIsUnavailable(t *testing.T) {
	resetBreakers()
	sick := newNode(t, 500, 0, "")
	c := meshClient(deadURL(t), sick.srv.URL, deadURL(t))
	_, err := c.Verify(context.Background(), "ak_x")
	if !errors.Is(err, ErrKeystoreUnavailable) || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("got %v", err)
	}
}

func TestVerify_ContextDeadlineIsUnavailable(t *testing.T) {
	resetBreakers()
	a := newNode(t, 200, time.Second, "a")
	b := newNode(t, 200, time.Second, "b")
	c := meshClient(a.srv.URL, b.srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := c.Verify(ctx, "ak_x"); !errors.Is(err, ErrKeystoreUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestBreakerMovesAFailingNodeToTheBack(t *testing.T) {
	resetBreakers()
	sick := newNode(t, 503, 0, "")
	peer := newNode(t, 200, 0, "bob")
	c := meshClient(sick.srv.URL, peer.srv.URL)
	for i := 0; i < breakerThreshold; i++ {
		if _, err := c.Verify(context.Background(), "ak_x"); err != nil {
			t.Fatal(err)
		}
	}
	before := sick.hits.Load()
	for i := 0; i < 5; i++ {
		if res, err := c.Verify(context.Background(), "ak_x"); err != nil || res.User != "bob" {
			t.Fatalf("%+v %v", res, err)
		}
	}
	if sick.hits.Load() != before {
		t.Fatalf("an open breaker should send traffic to the healthy node first (sick node got %d more)", sick.hits.Load()-before)
	}
	// it is still used when it is the only one left
	if got := c.orderedEndpoints(time.Now()); got[len(got)-1] != sick.srv.URL {
		t.Fatalf("order %v", got)
	}
}

func TestBreakerIgnoresOurOwnCancellations(t *testing.T) {
	resetBreakers()
	slow := newNode(t, 200, 500*time.Millisecond, "slow")
	fast := newNode(t, 200, 0, "fast")
	c := meshClient(slow.srv.URL, fast.srv.URL)
	for i := 0; i < breakerThreshold+2; i++ {
		if _, err := c.Verify(context.Background(), "ak_x"); err != nil {
			t.Fatal(err)
		}
	}
	if breakerFor(slow.srv.URL).isOpen(time.Now()) {
		t.Fatal("a node whose request we cancelled (because another answered first) must not be marked failing")
	}
}

func TestAdminFailover(t *testing.T) {
	resetBreakers()
	var issued atomic.Int64
	writer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/issue" {
			issued.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"key":"ak_new","revoked":true}}`))
	}))
	defer writer.Close()
	c := meshClient(deadURL(t), writer.URL)
	c.AdminToken = "adm"
	if res, err := c.Issue(context.Background(), IssueRequest{User: "u", Scope: "s"}); err != nil || res.Key != "ak_new" {
		t.Fatalf("issue across a refused node: %+v %v", res, err)
	}
	if ok, err := c.Revoke(context.Background(), "ak_old"); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if issued.Load() != 1 {
		t.Fatalf("issued %d times", issued.Load())
	}
}

func TestAdminIssueDoesNotRetryAfterTheRequestWasSent(t *testing.T) {
	resetBreakers()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // the client times out after sending
	}))
	defer slow.Close()
	var second atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { second.Add(1) }))
	defer other.Close()
	c := meshClient(slow.URL, other.URL)
	c.AdminToken = "adm"
	c.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond}
	if _, err := c.Issue(context.Background(), IssueRequest{User: "u"}); !errors.Is(err, ErrKeystoreUnavailable) {
		t.Fatalf("got %v", err)
	}
	if second.Load() != 0 {
		t.Fatal("issue must not be re-sent to another node once it may have reached the first (it could mint two keys)")
	}
}

func TestAdminHTTPErrorIsFinal(t *testing.T) {
	resetBreakers()
	var otherHits atomic.Int64
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer refuse.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherHits.Add(1) }))
	defer other.Close()
	c := meshClient(refuse.URL, other.URL)
	c.AdminToken = "adm"
	if _, err := c.List(context.Background()); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("got %v", err)
	}
	if otherHits.Load() != 0 {
		t.Fatal("an HTTP answer is final; the admin call must not move on")
	}
}

func TestParseEndpointList(t *testing.T) {
	got, rejected := ParseEndpointList(strings.Join([]string{
		"http://host.docker.internal:18021",
		"http://127.0.0.1:18021/",
		"http://10.10.10.20:18021",
		"http://100.92.211.122:18021",
		"http://apikey-service:8080",
		"https://apikey-service.0exec.com",
		"https://keystore.0infer.com:8443",
		"http://host.docker.internal:18021", // duplicate
		// rejected:
		"http://8.8.8.8:18021",
		"http://keys.example.com",
		"https://keys.example.com",
		"https://user:pw@apikey-service.0exec.com",
		"https://apikey-service.0exec.com/verify",
		"https://apikey-service.0exec.com?x=1",
		"http://[2001:db8::1]:18021",
		"ftp://10.0.0.1",
		"https://0exec.com.evil.net",
	}, ", "))
	want := []string{
		"http://host.docker.internal:18021",
		"http://127.0.0.1:18021",
		"http://10.10.10.20:18021",
		"http://100.92.211.122:18021",
		"http://apikey-service:8080",
		"https://apikey-service.0exec.com",
		"https://keystore.0infer.com:8443",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("accepted\n got %v\nwant %v", got, want)
	}
	if len(rejected) != 9 {
		t.Fatalf("rejected %d: %v", len(rejected), rejected)
	}
}

func TestNewReadsTheEndpointList(t *testing.T) {
	t.Setenv("APIKEY_SERVICE_URL", "http://10.10.10.20:18021")
	t.Setenv("APIKEY_SERVICE_URLS", "http://host.docker.internal:18021, http://8.8.8.8, https://apikey-service.0exec.com")
	t.Setenv("APIKEY_SERVICE_HEDGE_MS", "120")
	c := New()
	if c.BaseURL != "http://host.docker.internal:18021" || len(c.Endpoints) != 2 || c.HedgeDelay != 120*time.Millisecond {
		t.Fatalf("%+v", c)
	}
	t.Setenv("APIKEY_SERVICE_URLS", "")
	c = New()
	if c.BaseURL != "http://10.10.10.20:18021" || len(c.Endpoints) != 0 {
		t.Fatalf("without the list New must behave as before: %+v", c)
	}
	t.Setenv("APIKEY_SERVICE_URLS", "http://8.8.8.8")
	if c = New(); c.BaseURL != "http://10.10.10.20:18021" || len(c.Endpoints) != 0 {
		t.Fatalf("a list with no acceptable entry must not replace APIKEY_SERVICE_URL: %+v", c)
	}
}
