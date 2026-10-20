package fleetfetch

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// bigBody is a response body n bytes long.
func bigBody(n int64) []byte { return bytes.Repeat([]byte("x"), int(n)) }

// cacheServing is a fetch cache that answers every request with body, marked as
// an origin replay so that fleetfetch returns it as it is.
func cacheServing(body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-FetchCache-Fetched-At", time.Now().UTC().Format(time.RFC3339))
		w.Header().Set("X-FetchCache-Final-Url", "https://target.example/")
		_, _ = w.Write(body)
	}))
}

// originServing is an origin for the direct path.
func originServing(body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
}

// getVia fetches with c over the path named: "cache" through a fake cache serving
// body, "direct" straight from an origin serving it (WithoutCache).
func getVia(t *testing.T, path string, body []byte, opts ...Option) *Response {
	t.Helper()
	var (
		srv *httptest.Server
		c   *Client
		url string
	)
	switch path {
	case "cache":
		srv = cacheServing(body)
		c = NewClient(append([]Option{WithCacheURL(srv.URL), WithTimeout(10 * time.Second)}, opts...)...)
		url = "https://target.example/"
	case "direct":
		srv = originServing(body)
		c = NewClient(append([]Option{WithoutCache(), WithTimeout(10 * time.Second), WithFallbackClient(srv.Client())}, opts...)...)
		url = srv.URL
	default:
		t.Fatalf("unknown path %q", path)
	}
	defer srv.Close()
	r, err := c.Get(context.Background(), url)
	if err != nil {
		t.Fatalf("%s Get: %v", path, err)
	}
	return r
}

func TestDefaultBodyLimitIs32MiB(t *testing.T) {
	if DefaultMaxBodyBytes != 32<<20 {
		t.Fatalf("DefaultMaxBodyBytes = %d, want 32 MiB", DefaultMaxBodyBytes)
	}
	t.Setenv(EnvMaxBodyBytes, "")
	if got := NewClient().bodyLimit(); got != DefaultMaxBodyBytes {
		t.Fatalf("a client with no option and no environment has limit %d, want %d", got, DefaultMaxBodyBytes)
	}
	if got := (&Client{}).bodyLimit(); got != DefaultMaxBodyBytes {
		t.Fatalf("a Client not built by NewClient has limit %d, want the default", got)
	}
}

// Both paths, at the real default: a body one byte over is cut, a body of exactly
// the limit is not, and the client without options is the one that is bounded.
func TestDefaultBodyLimitAppliesOnBothPaths(t *testing.T) {
	t.Setenv(EnvMaxBodyBytes, "")
	for _, path := range []string{"cache", "direct"} {
		t.Run(path, func(t *testing.T) {
			r := getVia(t, path, bigBody(DefaultMaxBodyBytes+1))
			if !r.Truncated || int64(len(r.Body)) != DefaultMaxBodyBytes {
				t.Fatalf("a body of limit+1 bytes: Truncated=%v len=%d, want true and %d", r.Truncated, len(r.Body), DefaultMaxBodyBytes)
			}
			r = getVia(t, path, bigBody(DefaultMaxBodyBytes))
			if r.Truncated || int64(len(r.Body)) != DefaultMaxBodyBytes {
				t.Fatalf("a body of exactly the limit: Truncated=%v len=%d, want false and %d", r.Truncated, len(r.Body), DefaultMaxBodyBytes)
			}
			if r.Status != 200 {
				t.Fatalf("status %d: a cut body must not turn the fetch into an error", r.Status)
			}
		})
	}
}

func TestExplicitLimitTruncatesAndFlagsOnBothPaths(t *testing.T) {
	t.Setenv(EnvMaxBodyBytes, "")
	for _, path := range []string{"cache", "direct"} {
		t.Run(path, func(t *testing.T) {
			r := getVia(t, path, []byte("abcdefgh"), WithMaxBodyBytes(4))
			if string(r.Body) != "abcd" || !r.Truncated {
				t.Fatalf("body %q Truncated=%v, want abcd and true", r.Body, r.Truncated)
			}
			r = getVia(t, path, []byte("abcd"), WithMaxBodyBytes(4))
			if string(r.Body) != "abcd" || r.Truncated {
				t.Fatalf("a body of exactly the limit: %q Truncated=%v, want abcd and false", r.Body, r.Truncated)
			}
			r = getVia(t, path, []byte("abc"), WithMaxBodyBytes(4))
			if string(r.Body) != "abc" || r.Truncated {
				t.Fatalf("a short body: %q Truncated=%v", r.Body, r.Truncated)
			}
			// The pattern 23 fleet services use: ask for one byte more than they accept.
			r = getVia(t, path, []byte("abcdefgh"), WithMaxBodyBytes(5))
			if len(r.Body) != 5 {
				t.Fatalf("limit+1 pattern: len %d, want 5", len(r.Body))
			}
		})
	}
}

func TestWithUnboundedBodyReadsEverythingOnBothPaths(t *testing.T) {
	t.Setenv(EnvMaxBodyBytes, "")
	for _, path := range []string{"cache", "direct"} {
		t.Run(path, func(t *testing.T) {
			r := getVia(t, path, bigBody(DefaultMaxBodyBytes+10), WithUnboundedBody())
			if r.Truncated || int64(len(r.Body)) != DefaultMaxBodyBytes+10 {
				t.Fatalf("Truncated=%v len=%d, want the whole %d bytes", r.Truncated, len(r.Body), DefaultMaxBodyBytes+10)
			}
		})
	}
}

func TestBodyLimitEnvironmentOverride(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int64
	}{
		{"", DefaultMaxBodyBytes},
		{"  ", DefaultMaxBodyBytes},
		{"1024", 1024},
		{" 2048 ", 2048},
		{"0", unboundedBody},
		{"-1", DefaultMaxBodyBytes},
		{"abc", DefaultMaxBodyBytes},
		{"1.5", DefaultMaxBodyBytes},
		{"99999999999999999999", DefaultMaxBodyBytes},
	} {
		t.Setenv(EnvMaxBodyBytes, tc.env)
		if got := NewClient().maxBodyBytes; got != tc.want {
			t.Errorf("%s=%q: limit %d, want %d", EnvMaxBodyBytes, tc.env, got, tc.want)
		}
	}
}

func TestBodyLimitEnvironmentAppliesOnBothPaths(t *testing.T) {
	t.Setenv(EnvMaxBodyBytes, "4")
	for _, path := range []string{"cache", "direct"} {
		if r := getVia(t, path, []byte("abcdefgh")); string(r.Body) != "abcd" || !r.Truncated {
			t.Errorf("%s with %s=4: %q Truncated=%v", path, EnvMaxBodyBytes, r.Body, r.Truncated)
		}
	}
	t.Setenv(EnvMaxBodyBytes, "0")
	for _, path := range []string{"cache", "direct"} {
		if r := getVia(t, path, bigBody(DefaultMaxBodyBytes+1)); r.Truncated || int64(len(r.Body)) != DefaultMaxBodyBytes+1 {
			t.Errorf("%s with %s=0: Truncated=%v len=%d, want the whole body", path, EnvMaxBodyBytes, r.Truncated, len(r.Body))
		}
	}
}

func TestBodyLimitOptionsBeatEnvironmentAndNonPositiveIsIgnored(t *testing.T) {
	t.Setenv(EnvMaxBodyBytes, "100")
	if got := NewClient(WithMaxBodyBytes(7)).maxBodyBytes; got != 7 {
		t.Errorf("WithMaxBodyBytes(7) with the environment at 100: %d, want 7", got)
	}
	if got := NewClient(WithUnboundedBody()).maxBodyBytes; got != unboundedBody {
		t.Errorf("WithUnboundedBody with the environment at 100: %d, want unbounded", got)
	}
	// A configuration value that was never set must not turn the limit off.
	for _, n := range []int64{0, -1, -1 << 40} {
		if got := NewClient(WithMaxBodyBytes(n)).maxBodyBytes; got != 100 {
			t.Errorf("WithMaxBodyBytes(%d): limit %d, want the environment's 100", n, got)
		}
	}
	t.Setenv(EnvMaxBodyBytes, "")
	if got := NewClient(WithMaxBodyBytes(0)).bodyLimit(); got != DefaultMaxBodyBytes {
		t.Errorf("WithMaxBodyBytes(0): limit %d, want the default", got)
	}
	// Last of the two wins.
	if got := NewClient(WithUnboundedBody(), WithMaxBodyBytes(9)).maxBodyBytes; got != 9 {
		t.Errorf("WithUnboundedBody then WithMaxBodyBytes(9): %d", got)
	}
	if got := NewClient(WithMaxBodyBytes(9), WithUnboundedBody()).maxBodyBytes; got != unboundedBody {
		t.Errorf("WithMaxBodyBytes(9) then WithUnboundedBody: %d", got)
	}
}

func TestReadBodyReadsNoMoreThanOneByteOverTheLimit(t *testing.T) {
	c := &Client{maxBodyBytes: 10}
	r := &countingReader{}
	data, truncated, err := c.readBody(r)
	if err != nil || !truncated || len(data) != 10 {
		t.Fatalf("readBody on an endless body: len=%d truncated=%v err=%v", len(data), truncated, err)
	}
	if r.n != 11 {
		t.Fatalf("read %d bytes of an endless body, want 11 (limit+1)", r.n)
	}
	for _, tc := range []struct {
		in        string
		limit     int64
		want      string
		truncated bool
	}{
		{"", 3, "", false},
		{"ab", 3, "ab", false},
		{"abc", 3, "abc", false},
		{"abcd", 3, "abc", true},
		{"abcdef", 1, "a", true},
	} {
		got, trunc, err := (&Client{maxBodyBytes: tc.limit}).readBody(strings.NewReader(tc.in))
		if err != nil || string(got) != tc.want || trunc != tc.truncated {
			t.Errorf("readBody(%q, limit %d) = %q, %v, %v; want %q, %v", tc.in, tc.limit, got, trunc, err, tc.want, tc.truncated)
		}
	}
}

type countingReader struct{ n int }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.n += len(p)
	return len(p), nil
}

func TestTruncationIsCountedAndObserved(t *testing.T) {
	obs := &captureObserver{}
	SetDefaultObserver(obs)
	defer SetDefaultObserver(nil)
	t.Setenv(EnvMaxBodyBytes, "")

	srv := cacheServing([]byte("abcdefgh"))
	defer srv.Close()
	c := NewClient(WithCacheURL(srv.URL), WithMaxBodyBytes(4))
	for i := 0; i < 2; i++ {
		if _, err := c.Get(context.Background(), "https://target.example/"); err != nil {
			t.Fatal(err)
		}
	}
	small := NewClient(WithCacheURL(srv.URL), WithMaxBodyBytes(100))
	if _, err := small.Get(context.Background(), "https://target.example/"); err != nil {
		t.Fatal(err)
	}
	if got := c.Stats().Truncated; got != 2 {
		t.Errorf("Stats().Truncated = %d, want 2", got)
	}
	if got := small.Stats().Truncated; got != 0 {
		t.Errorf("a client that cut nothing counts %d", got)
	}
	var cut, whole int
	for _, e := range obs.events {
		if e.Truncated {
			cut++
		} else {
			whole++
		}
	}
	if cut != 2 || whole != 1 {
		t.Errorf("events: %d truncated, %d whole, want 2 and 1 (%+v)", cut, whole, obs.events)
	}
}

func TestNewHTTPClientMarksATruncatedBody(t *testing.T) {
	t.Setenv(EnvMaxBodyBytes, "")
	srv := cacheServing([]byte("abcdefgh"))
	defer srv.Close()
	resp, err := NewHTTPClient(WithCacheURL(srv.URL), WithMaxBodyBytes(4)).Get("https://target.example/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "abcd" || resp.Header.Get("X-FetchCache-Truncated") != "true" {
		t.Fatalf("body %q header %q, want abcd and true", b, resp.Header.Get("X-FetchCache-Truncated"))
	}
	resp, err = NewHTTPClient(WithCacheURL(srv.URL), WithMaxBodyBytes(100)).Get("https://target.example/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-FetchCache-Truncated") != "" {
		t.Fatal("an uncut body must not carry the truncation header")
	}
}
