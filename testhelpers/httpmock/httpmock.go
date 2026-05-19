// Package httpmock provides canonical httptest.Server factories for
// common fleet upstream patterns. It removes the boilerplate of wiring
// up httptest.NewServer, setting response codes, and asserting call counts.
//
// Usage:
//
//	// a server that always returns 200 JSON:
//	srv := httpmock.JSON(t, 200, map[string]any{"ok": true})
//	defer srv.Close()
//
//	// a server with per-request routing:
//	srv := httpmock.New(t).
//	    Handle("/verify", httpmock.RespondJSON(200, verifyResp)).
//	    Handle("/health", httpmock.RespondStatus(200)).
//	    Build()
//	defer srv.Close()
//
//	// assert the number of calls to a path:
//	srv.AssertCalls(t, "/verify", 1)
package httpmock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Server wraps httptest.Server and records per-path call counts.
type Server struct {
	*httptest.Server
	mu    sync.Mutex
	calls map[string]int
}

// CallCount returns the number of requests received for path.
func (s *Server) CallCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

// AssertCalls fails t if the number of calls to path does not equal want.
func (s *Server) AssertCalls(t testing.TB, path string, want int) {
	t.Helper()
	got := s.CallCount(path)
	if got != want {
		t.Errorf("httpmock: %s called %d times, want %d", path, got, want)
	}
}

// AssertAtLeast fails t if the number of calls to path is less than min.
func (s *Server) AssertAtLeast(t testing.TB, path string, min int) {
	t.Helper()
	got := s.CallCount(path)
	if got < min {
		t.Errorf("httpmock: %s called %d times, want at least %d", path, got, min)
	}
}

// ─── Simple constructors ──────────────────────────────────────────────────

// JSON creates a test server that responds to all requests with the given
// status code and JSON body. t.Cleanup registers server Close automatically.
func JSON(t testing.TB, status int, body any) *Server {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("httpmock.JSON: marshal: %v", err)
	}
	return build(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(b)
	}))
}

// Status creates a test server that responds to all requests with the
// given HTTP status code and no body.
func Status(t testing.TB, status int) *Server {
	t.Helper()
	return build(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
}

// Sequence creates a test server that responds to successive requests with
// the provided responses in order. When the sequence is exhausted, the
// last response is repeated.
func Sequence(t testing.TB, responses ...Response) *Server {
	t.Helper()
	var mu sync.Mutex
	i := 0
	return build(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		resp := responses[i]
		if i < len(responses)-1 {
			i++
		}
		mu.Unlock()
		resp.serve(w)
	}))
}

// ─── Builder ──────────────────────────────────────────────────────────────

// Builder constructs a Server with per-path handlers.
type Builder struct {
	t        testing.TB
	mux      *http.ServeMux
	fallback http.Handler
}

// New returns a Builder.
func New(t testing.TB) *Builder {
	return &Builder{t: t, mux: http.NewServeMux()}
}

// Handle registers a handler for the given pattern (same syntax as
// http.ServeMux).
func (b *Builder) Handle(pattern string, h http.Handler) *Builder {
	b.mux.Handle(pattern, h)
	return b
}

// HandleFunc registers a handler function for the given pattern.
func (b *Builder) HandleFunc(pattern string, fn http.HandlerFunc) *Builder {
	b.mux.HandleFunc(pattern, fn)
	return b
}

// WithFallback sets the handler for paths not matched by any registered
// pattern. Default: 404.
func (b *Builder) WithFallback(h http.Handler) *Builder {
	b.fallback = h
	return b
}

// Build creates the test server and registers t.Cleanup(srv.Close).
func (b *Builder) Build() *Server {
	b.t.Helper()
	if b.fallback != nil {
		b.mux.Handle("/", b.fallback)
	}
	return build(b.t, b.mux)
}

// ─── Response helpers ─────────────────────────────────────────────────────

// Response describes a canned HTTP response.
type Response struct {
	Status int
	Body   []byte
	Header http.Header
}

// RespondJSON creates a Response with a JSON body.
func RespondJSON(status int, body any) Response {
	b, _ := json.Marshal(body)
	return Response{Status: status, Body: b,
		Header: http.Header{"Content-Type": {"application/json"}}}
}

// RespondStatus creates a Response with no body.
func RespondStatus(status int) Response {
	return Response{Status: status}
}

func (resp Response) serve(w http.ResponseWriter) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if resp.Status != 0 {
		w.WriteHeader(resp.Status)
	}
	if len(resp.Body) > 0 {
		w.Write(resp.Body)
	}
}

// ─── internal ─────────────────────────────────────────────────────────────

func build(t testing.TB, h http.Handler) *Server {
	t.Helper()
	srv := &Server{calls: make(map[string]int)}
	recording := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.mu.Lock()
		srv.calls[r.URL.Path]++
		srv.mu.Unlock()
		h.ServeHTTP(w, r)
	})
	srv.Server = httptest.NewServer(recording)
	t.Cleanup(srv.Close)
	return srv
}
