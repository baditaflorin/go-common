package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/config"
)

func routeTestServer(opts ...Option) *Server {
	return New(&config.Config{AppName: "test", Version: "0.0.0", Port: "0"}, opts...)
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func TestValidateRoutes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prefixes []string
		register func(s *Server)
		wantErr  []string // substrings of the error; none means no error
	}{
		{
			name:     "no prefix declared, nothing to check",
			register: func(s *Server) { s.HandleFunc("/c/admin", ok) },
		},
		{
			name:     "empty and root prefixes are ignored, as the gate ignores them",
			prefixes: []string{"", "/"},
			register: func(s *Server) { s.HandleFunc("/anything", ok); s.HandleFunc("/", ok) },
		},
		{
			name:     "a gated route under the prefix",
			prefixes: []string{"/c/"},
			register: func(s *Server) { s.HandleFunc("/c/admin", ok) },
			wantErr:  []string{`route "/c/admin" lies under the public path prefix "/c/"`, "HandlePublic"},
		},
		{
			name:     "the prefix itself, registered as gated",
			prefixes: []string{"/c/"},
			register: func(s *Server) { s.HandleFunc("/c/", ok) },
			wantErr:  []string{`route "/c/" lies under the public path prefix "/c/"`},
		},
		{
			name:     "a pattern with a method and a wildcard",
			prefixes: []string{"/c/"},
			register: func(s *Server) { s.HandleFunc("POST /c/{token}/stats", ok) },
			wantErr:  []string{`route "POST /c/{token}/stats" lies under the public path prefix "/c/"`},
		},
		{
			name:     "a pattern with a host",
			prefixes: []string{"/c/"},
			register: func(s *Server) { s.HandleFunc("example.com/c/admin", ok) },
			wantErr:  []string{`route "example.com/c/admin" lies under`},
		},
		{
			name:     "an exact route at the prefix without its slash",
			prefixes: []string{"/c/"},
			register: func(s *Server) { s.HandleFunc("/c", ok) },
			wantErr:  []string{`route "/c" is the public path prefix "/c/" without its trailing slash`},
		},
		{
			name:     "a prefix without a trailing slash covers its siblings",
			prefixes: []string{"/c"},
			register: func(s *Server) { s.HandleFunc("/config", ok); s.HandleFunc("/cache/{key}", ok) },
			wantErr:  []string{`route "/cache/{key}" lies under the public path prefix "/c"`, `route "/config" lies under the public path prefix "/c"`},
		},
		{
			name:     "public routes under the prefix are what it is for",
			prefixes: []string{"/c/"},
			register: func(s *Server) {
				s.HandlePublic("/c/", http.HandlerFunc(ok))
				s.HandlePublicFunc("GET /c/{token}/{id}", ok)
			},
		},
		{
			name:     "routes outside the prefix, and routes that merely match more",
			prefixes: []string{"/c/"},
			register: func(s *Server) {
				s.HandleFunc("/c2/x", ok)
				s.HandleFunc("/ca", ok)
				s.HandleFunc("/hits", ok)
				s.HandleFunc("/", ok)
				s.HandleFunc("/{x}", ok)
				s.HandleFunc("/x/c/tok", ok)
			},
		},
		{
			name:     "a route registered directly on the mux is not recorded",
			prefixes: []string{"/c/"},
			register: func(s *Server) { s.Mux.HandleFunc("/c/admin", ok) },
		},
		{
			name:     "every conflict is reported, in order",
			prefixes: []string{"/c/", "/hooks/"},
			register: func(s *Server) {
				s.HandleFunc("/hooks/list", ok)
				s.HandleFunc("/c/admin", ok)
				s.HandleFunc("/fine", ok)
			},
			wantErr: []string{"2 keystore-gated route(s)", `"/c/admin" lies under the public path prefix "/c/"`, `"/hooks/list" lies under the public path prefix "/hooks/"`},
		},
		{
			name:     "the server's own gated mounts count",
			prefixes: []string{"/sch"},
			register: func(s *Server) {},
			wantErr:  []string{`route "/schema", which the server mounts itself, lies under the public path prefix "/sch"`},
		},
		{
			name:     "the server's own readyz and metrics routes count",
			prefixes: []string{"/readyz", "/metrics/"},
			register: func(s *Server) {},
			wantErr:  []string{`route "/metrics/json", which the server mounts itself,`, `route "/readyz", which the server mounts itself,`},
		},
		{
			name:     "the routes the gate always exempts are not the server's to guard",
			prefixes: []string{"/health", "/version", "/capabilities"},
			register: func(s *Server) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := routeTestServer(WithKeystoreAuthPublicPrefixes(tc.prefixes))
			if tc.prefixes == nil {
				s = routeTestServer()
			}
			tc.register(s)
			err := s.validateRoutes()
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("validateRoutes() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateRoutes() = nil, want an error mentioning %q", tc.wantErr)
			}
			msg := err.Error()
			at := 0
			for _, want := range tc.wantErr {
				i := strings.Index(msg[at:], want)
				if i < 0 {
					t.Fatalf("validateRoutes() = %q, want it to mention %q (after offset %d)", msg, want, at)
				}
				at += i
			}
		})
	}
}

func TestValidateRoutesCancelRoute(t *testing.T) {
	s := routeTestServer(WithKeystoreAuthPublicPrefixes([]string{"/cancel/"}), WithCancellationRegistry(CancellationConfig{}))
	err := s.validateRoutes()
	if err == nil || !strings.Contains(err.Error(), `route "/cancel/", which the server mounts itself, lies under the public path prefix "/cancel/"`) {
		t.Fatalf("validateRoutes() = %v, want the cancel route reported", err)
	}
}

func TestValidateRoutesAccumulatesPrefixesFromEveryDeclaration(t *testing.T) {
	s := routeTestServer(WithKeystoreAuthPublicPrefixes([]string{"/a/"}), WithKeystoreAuthPublicPrefixes([]string{"/b/"}))
	s.HandleFunc("/a/x", ok)
	s.HandleFunc("/b/x", ok)
	err := s.validateRoutes()
	if err == nil || !strings.Contains(err.Error(), "2 keystore-gated route(s)") {
		t.Fatalf("validateRoutes() = %v, want both routes reported", err)
	}
}

func TestPatternLiteralPath(t *testing.T) {
	for pattern, want := range map[string]string{
		"/c/":                    "/c/",
		"/c/admin":               "/c/admin",
		"/c/{token}":             "/c/",
		"/c/{token}/{id}":        "/c/",
		"/c/{token...}":          "/c/",
		"/c/{$}":                 "/c/",
		"GET /c/{token}":         "/c/",
		"POST   /c/x":            "/c/x",
		"example.com/c/x":        "/c/x",
		"GET example.com/c/{id}": "/c/",
		"/{x}":                   "/",
		"/":                      "/",
		"/files/{path...}":       "/files/",
		"  /padded  ":            "/padded",
		"":                       "",
		"GET":                    "",
		"no-slash":               "",
		"HEAD example.com":       "",
		"/a/b/{c}/d":             "/a/b/",
	} {
		if got := patternLiteralPath(pattern); got != want {
			t.Errorf("patternLiteralPath(%q) = %q, want %q", pattern, got, want)
		}
	}
}

// TestStartRefusesAConflictingPrefix: Start returns the error, and does not
// listen, rather than panicking, exiting the process or serving the route with
// no key.
func TestStartRefusesAConflictingPrefix(t *testing.T) {
	s := routeTestServer(WithKeystoreAuthPublicPrefixes([]string{"/c/"}))
	s.HandlePublicFunc("/c/", ok)
	s.HandleFunc("/c/admin", ok)

	done := make(chan error, 1)
	go func() { done <- s.Start() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), `route "/c/admin" lies under the public path prefix "/c/"`) {
			t.Fatalf("Start() = %v, want the conflict", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start() did not return: it went on to listen with a gated route under a public prefix")
	}
}

// TestPublicPrefixEndToEnd runs the declared prefix through the whole server: the
// handler chain server.New builds, with http.ServeMux behind the gate. The public
// handler is served without a key, the four paths that used to be a way around the
// gate are refused, and everything else is gated as before.
func TestPublicPrefixEndToEnd(t *testing.T) {
	s := routeTestServer(WithKeystoreAuthPublicPrefixes([]string{"/c/"}, "local-test-key"))
	s.HandlePublicFunc("/c/", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "catch") })
	s.HandleFunc("/admin", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ADMIN-REACHED") })
	if err := s.validateRoutes(); err != nil {
		t.Fatalf("validateRoutes() = %v, want nil for a public handler under the prefix", err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(target string, hdr map[string]string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := cli.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	for _, target := range []string{"/c/../admin", "/c/%2e%2e/admin", "/c/x/../../admin", "/c/..%2fadmin"} {
		code, body := get(target, nil)
		if code != http.StatusBadRequest || strings.Contains(body, "ADMIN-REACHED") {
			t.Errorf("GET %s = %d %q, want 400", target, code, body)
		}
	}
	for target, want := range map[string]int{
		"/health":   200,
		"/c/":       200,
		"/c/tok":    200,
		"/c/tok/id": 200,
		"/health/":  401, // not the fixed path /health, and never exempt
		"/admin":    401,
		"/hits":     401,
		"/readyz":   401,
	} {
		if code, body := get(target, nil); code != want {
			t.Errorf("GET %s = %d %q, want %d", target, code, body, want)
		}
	}
	if code, body := get("/admin", map[string]string{"X-API-Key": "local-test-key"}); code != 200 || body != "ADMIN-REACHED" {
		t.Errorf("GET /admin with the local token = %d %q, want 200", code, body)
	}
}
