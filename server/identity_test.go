package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/meshresult"
	"github.com/baditaflorin/go-common/selftest"
)

const (
	identityTestName    = "go_identity_test"
	identityTestVersion = "1.2.3"
)

// assertIdentity fails unless h holds exactly one X-Service-Name and one
// X-Service-Version, with the given values.
func assertIdentity(t *testing.T, what string, h http.Header, name, version string) {
	t.Helper()
	if got := h.Values(header.ServiceName); len(got) != 1 || got[0] != name {
		t.Errorf("%s: %s = %q, want exactly [%q]", what, header.ServiceName, got, name)
	}
	if got := h.Values(header.ServiceVersion); len(got) != 1 || got[0] != version {
		t.Errorf("%s: %s = %q, want exactly [%q]", what, header.ServiceVersion, got, version)
	}
}

func identityTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	return New(&config.Config{AppName: identityTestName, Version: identityTestVersion, Port: "0"}, opts...)
}

// anyRedirect stands for the redirect http.ServeMux sends for a path missing its
// trailing slash, whose status code depends on the Go release (301, 307 in 1.26).
const anyRedirect = -1

// Every class of answer a server.New service sends names the service: the
// handler's own success and error answers, the mux's, the server's own routes,
// and the 400 that fetchSourceMiddleware writes before the chain runs.
func TestEveryResponseNamesTheService(t *testing.T) {
	srv := identityTestServer(t, WithMaxBodyBytes(8))
	srv.Mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv.Mux.HandleFunc("/created", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	srv.Mux.HandleFunc("/silent", func(http.ResponseWriter, *http.Request) {})
	srv.Mux.HandleFunc("/boom", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	})
	srv.Mux.HandleFunc("/unavailable", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	srv.Mux.HandleFunc("/mesh-error", func(w http.ResponseWriter, _ *http.Request) {
		meshresult.WriteOutcome(w, meshresult.OutcomeError, "upstream failed", nil)
	})
	srv.Mux.HandleFunc("/mesh-nodata", func(w http.ResponseWriter, _ *http.Request) {
		meshresult.WriteOutcome(w, meshresult.OutcomeNoData, "nothing found", nil)
	})
	srv.Mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv.Mux.HandleFunc("GET /only-get", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv.Mux.HandleFunc("/dir/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/ok", "", http.StatusOK},
		{http.MethodHead, "/ok", "", http.StatusOK},
		{http.MethodGet, "/created", "", http.StatusCreated},
		{http.MethodGet, "/silent", "", http.StatusOK},
		{http.MethodGet, "/boom", "", http.StatusInternalServerError},
		{http.MethodGet, "/unavailable", "", http.StatusServiceUnavailable},
		{http.MethodGet, "/mesh-error", "", http.StatusBadGateway},
		{http.MethodGet, "/mesh-nodata", "", http.StatusNotFound},
		{http.MethodPost, "/upload", "far more than eight bytes", http.StatusRequestEntityTooLarge},
		{http.MethodGet, "/no-such-route", "", http.StatusNotFound},
		{http.MethodPost, "/only-get", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/dir", "", anyRedirect},
		{http.MethodGet, "/ok?source=live&source=live", "", http.StatusBadRequest},
		{http.MethodGet, "/health", "", http.StatusOK},
		{http.MethodGet, "/version", "", http.StatusOK},
		{http.MethodGet, "/selftest", "", http.StatusOK},
		{http.MethodGet, "/metrics", "", http.StatusOK},
		{http.MethodGet, "/metrics/json", "", http.StatusOK},
		{http.MethodGet, "/readyz", "", http.StatusOK},
		{http.MethodGet, "/capabilities", "", http.StatusOK},
		{http.MethodGet, "/schema", "", http.StatusOK},
	} {
		what := tc.method + " " + tc.path
		req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if tc.want == anyRedirect {
			if resp.StatusCode < 300 || resp.StatusCode > 399 {
				t.Errorf("%s: status %d, want a redirect", what, resp.StatusCode)
			}
		} else if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", what, resp.StatusCode, tc.want)
		}
		assertIdentity(t, what, resp.Header, identityTestName, identityTestVersion)
	}
}

// A registered selftest.Suite answers through the chain, a failing one with 503.
func TestRegisteredSelftestNamesTheService(t *testing.T) {
	srv := identityTestServer(t)
	suite := selftest.NewSuite(identityTestName, identityTestVersion)
	suite.Check("always fails", func(context.Context) error { return errors.New("no") })
	srv.Mux.Handle("/selftest", suite.Handler())
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/selftest", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing selftest: status %d, want 503", rr.Code)
	}
	assertIdentity(t, "failing /selftest", rr.Header(), identityTestName, identityTestVersion)
}

// The keystore gate's own refusals (401, 403) name the service too.
func TestKeystoreRefusalsNameTheService(t *testing.T) {
	keystore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(header.VerifyKey) != "identity-free-tier-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set(header.AuthUser, "identity-caller")
		w.Header().Set(header.AuthScope, "default")
		w.Header().Set(header.AuthTier, "free")
		w.WriteHeader(http.StatusOK)
	}))
	defer keystore.Close()
	t.Setenv("APIKEY_SERVICE_URL", keystore.URL)

	srv := identityTestServer(t, WithKeystoreAuthTier("vetted-pentest", true))
	srv.Mux.HandleFunc("/scan", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, tc := range []struct {
		key  string
		want int
	}{
		{"", http.StatusUnauthorized},
		{"identity-unknown-key", http.StatusUnauthorized},
		{"identity-free-tier-key", http.StatusForbidden},
	} {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/scan", nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.key != "" {
			req.Header.Set(header.APIKey, tc.key)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("key %q: status %d, want %d", tc.key, resp.StatusCode, tc.want)
		}
		assertIdentity(t, "key "+tc.key, resp.Header, identityTestName, identityTestVersion)
	}
}

// The server's values win over a handler's: one it set, added or deleted is
// replaced when the response starts, or when the handler returns without writing.
func TestHandlerCannotReplaceTheIdentity(t *testing.T) {
	srv := identityTestServer(t)
	srv.Mux.HandleFunc("/set", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(header.ServiceName, "someone-else")
		w.Header().Set(header.ServiceVersion, "9.9.9")
		_, _ = w.Write([]byte("x"))
	})
	srv.Mux.HandleFunc("/add", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add(header.ServiceName, "someone-else")
		w.Header().Add(header.ServiceVersion, "9.9.9")
		w.WriteHeader(http.StatusAccepted)
	})
	srv.Mux.HandleFunc("/del", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Del(header.ServiceName)
		w.Header().Del(header.ServiceVersion)
		http.Error(w, "gone", http.StatusGone)
	})
	srv.Mux.HandleFunc("/mutate", func(w http.ResponseWriter, _ *http.Request) {
		// Writes into the value slice the server preset.
		w.Header()[header.ServiceName][0] = "someone-else"
		w.Header()[header.ServiceVersion][0] = "9.9.9"
		w.WriteHeader(http.StatusOK)
	})
	srv.Mux.HandleFunc("/del-silent", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Del(header.ServiceName)
		w.Header().Set(header.ServiceVersion, "9.9.9")
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, path := range []string{"/set", "/add", "/del", "/mutate", "/del-silent"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		assertIdentity(t, path, resp.Header, identityTestName, identityTestVersion)
	}
}

// The fleet fetch cache runs server.New and replays the headers it stored from
// the service it fetched (go_infrastructure_fetch_cache writeEntry adds every
// stored header). Its answer names the cache, never the service whose answer it
// stored: a caller cannot tell a cached answer's producer from these headers
// (DomainScope ignores them on cache-served answers for that reason), and an
// origin cannot claim an identity through the cache.
func TestFetchCacheAnswerNamesTheCache(t *testing.T) {
	srv := New(&config.Config{AppName: fetchCacheServiceAppName, Version: "0.3.50", Port: "0"})
	stored := http.Header{}
	stored.Set("Content-Type", "application/json")
	stored.Set(header.ServiceName, "go_analyze_headers")
	stored.Set(header.ServiceVersion, "1.4.9")
	srv.Mux.HandleFunc("/fetch", func(w http.ResponseWriter, _ *http.Request) {
		for k, vs := range stored {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("X-FetchCache-Hit", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/fetch", nil))
	assertIdentity(t, "cache-served answer", rr.Header(), fetchCacheServiceAppName, "0.3.50")
	if rr.Header().Get("X-FetchCache-Hit") != "true" || rr.Header().Get("Content-Type") != "application/json" {
		t.Errorf("the replayed headers other than the identity must pass through: %v", rr.Header())
	}
}

// A value that cannot be a header value is not sent; with neither usable the
// handler is not wrapped at all.
func TestUnusableIdentityValuesAreNotSent(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, tc := range []struct {
		name, version         string
		wantName, wantVersion string
	}{
		{"svc", "", "svc", ""},
		{"", "1.0.0", "", "1.0.0"},
		{"  svc  ", " 1.0.0\t", "svc", "1.0.0"},
		{"svc\r\nX-Injected: 1", "1.0.0", "", "1.0.0"},
		{"svc", "1.0.0\x7f", "svc", ""},
		{"svc", strings.Repeat("9", maxIdentityValueLen+1), "svc", ""},
		{"svc", strings.Repeat("9", maxIdentityValueLen), "svc", strings.Repeat("9", maxIdentityValueLen)},
	} {
		rr := httptest.NewRecorder()
		identityMiddleware(tc.name, tc.version, ok).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		for key, want := range map[string]string{header.ServiceName: tc.wantName, header.ServiceVersion: tc.wantVersion} {
			got := rr.Header().Values(key)
			if want == "" && len(got) != 0 {
				t.Errorf("(%q, %q): %s = %q, want it absent", tc.name, tc.version, key, got)
			}
			if want != "" && (len(got) != 1 || got[0] != want) {
				t.Errorf("(%q, %q): %s = %q, want [%q]", tc.name, tc.version, key, got, want)
			}
		}
		if rr.Header().Get("X-Injected") != "" {
			t.Errorf("(%q, %q): a header was injected", tc.name, tc.version)
		}
	}
	h := identityMiddleware("", "\n", ok)
	if _, wrapped := h.(http.HandlerFunc); !wrapped {
		t.Fatalf("test assumption broken: the handler is not an http.HandlerFunc")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if len(rr.Header()) != 0 {
		t.Errorf("no usable value: headers %v, want none", rr.Header())
	}
}

// A streaming handler flushes through the wrapper (as a plain http.Flusher and
// through http.ResponseController), and the flushed header carries the identity.
func TestStreamingResponsesNameTheService(t *testing.T) {
	srv := identityTestServer(t)
	srv.Mux.HandleFunc("/stream", func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		f.Flush()
		_, _ = w.Write([]byte("data: 1\n\n"))
	})
	srv.Mux.HandleFunc("/stream-rc", func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("data: 1\n\n"))
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for _, path := range []string{"/stream", "/stream-rc"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "data: 1\n\n" {
			t.Errorf("%s: status %d body %q", path, resp.StatusCode, body)
		}
		assertIdentity(t, path, resp.Header, identityTestName, identityTestVersion)
	}
}

// A 1xx informational header does not count as the start of the response: the
// final response that follows it names the service.
func TestEarlyHintsThenFinalResponse(t *testing.T) {
	srv := identityTestServer(t)
	srv.Mux.HandleFunc("/hints", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set(header.ServiceVersion, "9.9.9")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	var got1xx int
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		got1xx = code
		return nil
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, ts.URL+"/hints", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got1xx != http.StatusEarlyHints || resp.StatusCode != http.StatusOK {
		t.Fatalf("got 1xx %d then %d, want 103 then 200", got1xx, resp.StatusCode)
	}
	assertIdentity(t, "final response after 103", resp.Header, identityTestName, identityTestVersion)
}

// http.ResponseController reaches the connection through the wrapper (Unwrap):
// a write deadline can be set and the connection hijacked.
func TestResponseControllerReachesTheConnection(t *testing.T) {
	h := identityMiddleware(identityTestName, identityTestVersion, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			http.Error(w, "deadline: "+err.Error(), http.StatusInternalServerError)
			return
		}
		conn, buf, err := rc.Hijack()
		if err != nil {
			http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nhijacked")
		_ = buf.Flush()
	}))
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hijacked" {
		t.Fatalf("body %q (status %d), want the hijacked connection's answer", body, resp.StatusCode)
	}
}

// The common response costs one allocation for the identity: the writer. A
// handler that keeps the preset values allocates no header slices.
func TestIdentityCostsOneAllocation(t *testing.T) {
	h := identityMiddleware(identityTestName, identityTestVersion, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := &reusableWriter{h: http.Header{}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(w, r) // first run sizes the map
	allocs := testing.AllocsPerRun(200, func() {
		clear(w.h)
		h.ServeHTTP(w, r)
	})
	if allocs > 1 {
		t.Fatalf("%v allocations per response, want at most 1", allocs)
	}
	assertIdentity(t, "reused writer", w.h, identityTestName, identityTestVersion)
}

// reusableWriter is a ResponseWriter whose header map survives across requests,
// so an allocation count sees only what the middleware allocates.
type reusableWriter struct{ h http.Header }

func (w *reusableWriter) Header() http.Header         { return w.h }
func (w *reusableWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *reusableWriter) WriteHeader(int)             {}

var benchBody = []byte("ok")

// BenchmarkIdentityMiddleware is the identity headers' own cost per response,
// around a handler that writes a status and two bytes into a reused writer. It
// is the "service identity" row of docs/observability.md; scripts/bench-gate.sh
// watches its allocations.
func BenchmarkIdentityMiddleware(b *testing.B) {
	h := identityMiddleware(identityTestName, identityTestVersion, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(benchBody)
	}))
	w := &reusableWriter{h: http.Header{}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clear(w.h)
		h.ServeHTTP(w, r)
	}
}
