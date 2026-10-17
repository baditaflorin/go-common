package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/server"
)

// promScrape fetches GET /metrics from the loopback server and returns the
// http_requests_total lines, the one family that carries the route label.
func promScrape(t *testing.T, base string) []string {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "http_requests_total{") {
			lines = append(lines, line)
		}
	}
	return lines
}

// promCount returns the value of the http_requests_total series for a route
// label ("" when there is none).
func promCount(lines []string, route string) string {
	for _, line := range lines {
		if strings.Contains(line, `route="`+route+`"`) {
			return line[strings.LastIndex(line, " ")+1:]
		}
	}
	return ""
}

func getAll(t *testing.T, base string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func newRouteTestServer(t *testing.T, app string, opts ...server.Option) *httptest.Server {
	t.Helper()
	srv := server.New(&config.Config{AppName: app, Version: "0.0.0", Port: "0"}, opts...)
	srv.Mux.HandleFunc("/things/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestWithPromRouteTemplatesTheRouteLabel: a route function reaches the
// collectors, so 30 distinct ids make one series, not 30.
func TestWithPromRouteTemplatesTheRouteLabel(t *testing.T) {
	ts := newRouteTestServer(t, "prom_route_fn", server.WithPromRoute(func(r *http.Request) string {
		if strings.HasPrefix(r.URL.Path, "/things/") {
			return "/things/{id}"
		}
		return r.URL.Path
	}, 0))

	var paths []string
	for i := 0; i < 30; i++ {
		paths = append(paths, "/things/"+strconv.Itoa(i))
	}
	getAll(t, ts.URL, paths...)

	lines := promScrape(t, ts.URL)
	if got := promCount(lines, "/things/{id}"); got != "30" {
		t.Errorf(`route="/things/{id}" = %q, want 30; series: %v`, got, lines)
	}
	if got := promCount(lines, "/things/7"); got != "" {
		t.Errorf("raw path /things/7 still has its own series (%s)", got)
	}
	if len(lines) != 1 {
		t.Errorf("%d http_requests_total series, want 1: %v", len(lines), lines)
	}
}

// TestWithPromRouteLimit: the limit alone changes the cap and leaves the raw
// path as the label, so the first N distinct paths keep a series and the rest
// fold into "_other".
func TestWithPromRouteLimit(t *testing.T) {
	ts := newRouteTestServer(t, "prom_route_limit", server.WithPromRoute(nil, 3))

	var paths []string
	for i := 0; i < 10; i++ {
		paths = append(paths, "/things/"+strconv.Itoa(i))
	}
	getAll(t, ts.URL, paths...)

	lines := promScrape(t, ts.URL)
	for _, p := range []string{"/things/0", "/things/1", "/things/2"} {
		if got := promCount(lines, p); got != "1" {
			t.Errorf("route=%q = %q, want 1", p, got)
		}
	}
	if got := promCount(lines, "_other"); got != "7" {
		t.Errorf(`route="_other" = %q, want 7`, got)
	}
	if got := promCount(lines, "/things/9"); got != "" {
		t.Errorf("beyond-cap /things/9 has its own series (%s)", got)
	}
}

// TestDefaultPromRouteIsUnchanged: without the option the route label is the
// raw path, even after another server in the same process opted in. The option
// is per server; it must not rewrite the shared collectors.
func TestDefaultPromRouteIsUnchanged(t *testing.T) {
	const app = "prom_route_default"
	_ = newRouteTestServer(t, app, server.WithPromRoute(func(*http.Request) string { return "/templated" }, 1))
	ts := newRouteTestServer(t, app)

	getAll(t, ts.URL, "/things/41", "/things/42")

	lines := promScrape(t, ts.URL)
	for _, p := range []string{"/things/41", "/things/42"} {
		if got := promCount(lines, p); got != "1" {
			t.Errorf("route=%q = %q, want 1 (default route label is the raw path); series: %v", p, got, lines)
		}
	}
	if got := promCount(lines, "/templated"); got != "" {
		t.Errorf("a server without WithPromRoute used another server's route function (%s)", got)
	}
}
