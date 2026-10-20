package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/apikey"
)

// nonCanonicalProbes are request targets that start with the public prefix "/c/"
// (or are a fixed public path) but name something else once a router has read
// them in one of the ways routers differ: dot segments, encoded dots and slashes,
// a double encoding, a backslash, an empty segment, a prefix that is only
// percent-encoded in the raw form.
var nonCanonicalProbes = []string{
	"/c/../admin",
	"/c/%2e%2e/admin",
	"/c/x/../../admin",
	"/c/..%2fadmin",
	"/c/%2E%2E/admin",
	"/c/..%2Fadmin",
	"/c/%252e%252e/admin",
	"/c/%2e./admin",
	"/c/.%2e/admin",
	"/c/./admin",
	"/c/tok/..",
	"/c/tok/../",
	"/c//admin",
	"/c/tok%2fid",
	"/c/tok%5cid",
	"/c/..%5cadmin",
	"/c/tok\\..\\admin",
	"/%63/admin",
	"/%63/tok",
	"/openapi%2ejson",
	"/agent%2Ejson",
	"/%68ealth",
}

// rejectKeys is a verifier that knows no key; it counts the calls it gets.
func rejectKeys() *stubVerifier {
	return &stubVerifier{verify: func(context.Context, string) (*apikey.VerifyResult, error) {
		return nil, apikey.ErrInvalidKey
	}}
}

// gatedRouters puts one gate in front of each of the two router shapes a service
// can have: net/http.ServeMux, which is what server.New uses, and a router that
// path.Clean()s the path before it routes. Both serve a gated /admin, a public
// catch-all under /c/ and /health.
func gatedRouters(gate Middleware) map[string]http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ADMIN-REACHED") })
	mux.HandleFunc("/c/", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "catch") })
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	cleaning := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = path.Clean(r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/") && r2.URL.Path != "/" {
			r2.URL.Path += "/" // as http.ServeMux does: a trailing slash is kept
		}
		r2.URL.RawPath = ""
		mux.ServeHTTP(w, r2)
	})
	return map[string]http.Handler{
		"http.ServeMux":             gate(mux),
		"path.Clean before routing": gate(cleaning),
	}
}

// get sends GET target to srv with the request target exactly as written (the
// client neither cleans nor decodes it) and never follows a redirect.
func get(t *testing.T, srv *httptest.Server, target string, hdr http.Header) (int, string) {
	t.Helper()
	parsed, err := url.ParseRequestURI(target)
	if err != nil {
		t.Fatalf("test target %q does not parse: %v", target, err)
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path, u.RawPath, u.RawQuery = parsed.Path, parsed.RawPath, parsed.RawQuery
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestKeystore_NonCanonicalPublicPathIsRefused is the regression test for the
// finding behind it: a path that starts with a declared public prefix but is not
// canonical used to skip the gate, and behind a router that cleans the path
// before routing it reached a keystore-gated route with no key. The gate now
// answers 400 itself, in front of either router shape, whatever credential the
// request carries, and never forwards it.
func TestKeystore_NonCanonicalPublicPathIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts KeystoreOpts
		hdr  http.Header
	}{
		{"no key", KeystoreOpts{}, nil},
		{"bad key", KeystoreOpts{}, http.Header{"X-Api-Key": {"nope"}}},
		{"local key", KeystoreOpts{LocalTokens: []string{"local-key"}}, http.Header{"X-Api-Key": {"local-key"}}},
		{"gateway header", KeystoreOpts{}, http.Header{"X-Auth-User": {"someone"}}},
	} {
		v := rejectKeys()
		tc.opts.Verifier = v
		tc.opts.PublicPathPrefixes = []string{"/c/"}
		for shape, h := range gatedRouters(TokenAuthKeystore(tc.opts)) {
			srv := httptest.NewServer(h)
			for _, target := range nonCanonicalProbes {
				code, body := get(t, srv, target, tc.hdr)
				if code != http.StatusBadRequest {
					t.Errorf("%s, %s: GET %s = %d %q, want 400", tc.name, shape, target, code, body)
				}
				if strings.Contains(body, "ADMIN-REACHED") || strings.Contains(body, "catch") || body == "ok" {
					t.Errorf("%s, %s: GET %s reached a handler: %q", tc.name, shape, target, body)
				}
				var reply map[string]string
				if err := json.Unmarshal([]byte(body), &reply); err != nil || reply["reason"] != "non-canonical path" {
					t.Errorf("%s, %s: GET %s body %q, want the JSON refusal", tc.name, shape, target, body)
				}
			}
			srv.Close()
		}
		if v.calls != 0 {
			t.Errorf("%s: the keystore was asked %d times; a refused path must not reach the credential checks", tc.name, v.calls)
		}
	}
}

// TestKeystore_CanonicalPublicPathsKeepWorking pins the other half: what the gate
// did for a canonical path before it still does, in front of both router shapes.
func TestKeystore_CanonicalPublicPathsKeepWorking(t *testing.T) {
	gate := TokenAuthKeystore(KeystoreOpts{Verifier: rejectKeys(), PublicPathPrefixes: []string{"/c/"}})
	for shape, h := range gatedRouters(gate) {
		srv := httptest.NewServer(h)
		for _, tc := range []struct {
			target string
			want   int
			body   string // substring of the body when the request is served
		}{
			{"/health", 200, "ok"},
			{"/c/", 200, "catch"},
			{"/c/tok", 200, "catch"},
			{"/c/tok/", 200, "catch"},
			{"/c/tok/id", 200, "catch"},
			{"/c/a.b_c-d~e", 200, "catch"},
			{"/c/tok%20x", 200, "catch"},
			{"/c/%41bc", 200, "catch"},
			{"/c/tok?x=../..&y=%2e%2e", 200, "catch"}, // the query is not part of the path
			// Not exempt, and not an error: gated exactly as before.
			{"/health/", 401, ""},
			{"/health/x", 401, ""},
			{"/admin", 401, ""},
			{"/", 401, ""},
			{"/c", 401, ""},
			{"/x/c/tok", 401, ""},
			{"/admin/../health", 401, ""}, // names no public path as written
			{"/a/..%2fb", 401, ""},
		} {
			code, body := get(t, srv, tc.target, nil)
			if code != tc.want || !strings.Contains(body, tc.body) {
				t.Errorf("%s: GET %s = %d %q, want %d containing %q", shape, tc.target, code, body, tc.want, tc.body)
			}
		}
		srv.Close()
	}
}

// TestKeystore_ProbePathsNeverReachAGatedRoute runs the four paths from the
// review against both router shapes and says what must hold whatever the status:
// the gated route is not reached.
func TestKeystore_ProbePathsNeverReachAGatedRoute(t *testing.T) {
	gate := TokenAuthKeystore(KeystoreOpts{Verifier: rejectKeys(), PublicPathPrefixes: []string{"/c/"}})
	for shape, h := range gatedRouters(gate) {
		srv := httptest.NewServer(h)
		for _, target := range []string{"/c/../admin", "/c/%2e%2e/admin", "/c/x/../../admin", "/c/..%2fadmin"} {
			code, body := get(t, srv, target, nil)
			if code != http.StatusUnauthorized && code != http.StatusBadRequest {
				t.Errorf("%s: GET %s = %d %q, want 401 or 400", shape, target, code, body)
			}
			if strings.Contains(body, "ADMIN-REACHED") {
				t.Errorf("%s: GET %s reached the gated /admin with no key", shape, target)
			}
		}
		srv.Close()
	}
}

// TestKeystore_NotExemptPathsAreAuthenticatedAsBefore: the refusal is for a path
// that names a public route in a non-canonical form, not for non-canonical paths in
// general. A request that is not exempt is authenticated as before, whatever its
// path looks like, and a good key gets it to the router.
func TestKeystore_NotExemptPathsAreAuthenticatedAsBefore(t *testing.T) {
	v := &stubVerifier{verify: func(_ context.Context, key string) (*apikey.VerifyResult, error) {
		if key != "good" {
			return nil, apikey.ErrInvalidKey
		}
		return &apikey.VerifyResult{User: "u", Scope: "s"}, nil
	}}
	gate := TokenAuthKeystore(KeystoreOpts{Verifier: v, PublicPathPrefixes: []string{"/c/"}})
	hdr := http.Header{"X-Api-Key": {"good"}}
	for shape, h := range gatedRouters(gate) {
		srv := httptest.NewServer(h)
		if code, body := get(t, srv, "/admin", hdr); code != 200 || body != "ADMIN-REACHED" {
			t.Errorf("%s: GET /admin with a good key = %d %q", shape, code, body)
		}
		// The gate is not what answers this: the router decides what it means.
		if code, body := get(t, srv, "/x/../admin", hdr); code == http.StatusBadRequest {
			t.Errorf("%s: GET /x/../admin with a good key was refused by the gate: %q", shape, body)
		}
		if code, _ := get(t, srv, "/x/../admin", nil); code == 200 {
			t.Errorf("%s: GET /x/../admin with no key = 200", shape)
		}
		srv.Close()
	}
}

// TestKeystore_NonCanonicalRefusalIsObserved: the refusal is visible to the
// operator as AuthSourceBypass + AuthResultDeny (apikey_auth_total{source="bypass",
// result="deny"}), a combination nothing else produces, so it can be alerted on.
func TestKeystore_NonCanonicalRefusalIsObserved(t *testing.T) {
	var events []AuthEvent
	v := rejectKeys()
	mw := TokenAuthKeystore(KeystoreOpts{
		Verifier:           v,
		PublicPathPrefixes: []string{"/c/"},
		Observer:           observerFunc(func(e AuthEvent) { events = append(events, e) }),
	})
	code, _ := run(t, mw, newReq("/c/%2e%2e/admin"))
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
	if len(events) != 1 || events[0].Source != AuthSourceBypass || events[0].Result != AuthResultDeny {
		t.Fatalf("events = %+v, want one bypass/deny", events)
	}
	events = nil
	if code, _ := run(t, mw, newReq("/c/tok")); code != http.StatusOK {
		t.Fatalf("canonical public path = %d, want 200", code)
	}
	if len(events) != 1 || events[0].Source != AuthSourceBypass || events[0].Result != AuthResultAllow {
		t.Fatalf("events = %+v, want one bypass/allow", events)
	}
	if v.calls != 0 {
		t.Fatalf("keystore calls = %d, want 0", v.calls)
	}
}

// TestKeystore_FixedPathsOnlyWhenCanonical: the fixed list is held to the same rule.
func TestKeystore_FixedPathsOnlyWhenCanonical(t *testing.T) {
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: rejectKeys()})
	for target, want := range map[string]int{
		"/health":         200,
		"/version":        200,
		"/selftest":       200,
		"/_gw_health":     200,
		"/capabilities":   200,
		"/openapi.json":   200,
		"/agent.json":     200,
		"/openapi%2ejson": 400, // the decoded path is /openapi.json, but the dot was sent encoded
		"/agent%2Ejson":   400,
		"/%68ealth":       400, // decodes to /health; the raw path is something else
		"/health/":        401, // not a fixed path, and never was
		"/health/..":      401, // names no fixed path as written
	} {
		if got, _ := run(t, mw, newReq(target)); got != want {
			t.Errorf("GET %s = %d, want %d", target, got, want)
		}
	}
}

// TestKeystore_PublicPrefixThatNeedsEscaping: a prefix with a character that is
// sent percent-encoded works, in either case of the hex digits, and is held to the
// canonical-path rule like any other.
func TestKeystore_PublicPrefixThatNeedsEscaping(t *testing.T) {
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: rejectKeys(), PublicPathPrefixes: []string{"/café/"}})
	for target, want := range map[string]int{
		"/caf%C3%A9/tok":      200,
		"/caf%c3%a9/tok":      200,
		"/cafe/tok":           401,
		"/caf%C3%A9/..%2fx":   400,
		"/caf%C3%A9/tok/../x": 400,
		"/caf%C3%A9/%2e%2e/x": 400,
		"/caf%C3%A9/tok%2fid": 400,
	} {
		if got, _ := run(t, mw, newReq(target)); got != want {
			t.Errorf("GET %s = %d, want %d", target, got, want)
		}
	}
}

func TestIsCanonicalPath(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   bool
	}{
		{"/", true},
		{"/health", true},
		{"/c/", true},
		{"/c/tok", true},
		{"/c/tok/", true},
		{"/c/tok/id", true},
		{"/c/a.b/c.d", true},
		{"/c/..a/b..", true}, // dots inside a segment are not a dot segment
		{"/c/...", true},
		{"/c/%41", true},
		{"/c/tok%20x", true},
		{"/c/tok%25", true}, // an encoded percent sign
		{"/c/tok;x=../y", true},
		{"/c/a:b@c,d+e=f", true},

		{"/c//", false},
		{"//", false},
		{"//c/x", false},
		{"/c//x", false},
		{"/c/.", false},
		{"/c/./", false},
		{"/c/..", false},
		{"/c/../", false},
		{"/c/x/..", false},
		{"/c/x/../y", false},
		{"/..", false},
		{"/c/%2e", false},
		{"/c/%2E", false},
		{"/c/%2e%2e", false},
		{"/c/.%2e/x", false},
		{"/c/a%2fb", false},
		{"/c/a%2Fb", false},
		{"/c/a%5cb", false},
		{"/c/a%5Cb", false},
		{"/c/%252e%252e/x", false}, // decodes to the text %2e%2e: a second decode reads ".."
		{"/c/%252f", false},
		{"/c/%255c", false},
		{"/c/a\\b", false},
	} {
		u, err := url.ParseRequestURI(tc.target)
		if err != nil {
			t.Fatalf("%q does not parse: %v", tc.target, err)
		}
		if got := isCanonicalPath(u); got != tc.want {
			t.Errorf("isCanonicalPath(%q) = %v, want %v (Path %q, escaped %q)", tc.target, got, tc.want, u.Path, u.EscapedPath())
		}
	}
	if isCanonicalPath(&url.URL{}) || isCanonicalPath(&url.URL{Path: "c/tok"}) {
		t.Error("an empty or relative path must not be canonical")
	}
}

func TestMatchPublicPathPrefix(t *testing.T) {
	prefixes := []string{"", "/", "/c/", "/hooks/"}
	for _, tc := range []struct {
		path   string
		prefix string
		ok     bool
	}{
		{"/c/tok", "/c/", true},
		{"/c/", "/c/", true},
		{"/hooks/a/b", "/hooks/", true},
		{"/c", "", false},
		{"/", "", false},
		{"/other", "", false},
		{"", "", false},
	} {
		got, ok := MatchPublicPathPrefix(tc.path, prefixes)
		if got != tc.prefix || ok != tc.ok {
			t.Errorf("MatchPublicPathPrefix(%q) = %q, %v; want %q, %v", tc.path, got, ok, tc.prefix, tc.ok)
		}
	}
	if _, ok := MatchPublicPathPrefix("/anything", nil); ok {
		t.Error("no prefixes must match nothing")
	}
}

// TestCanonicalBypassCostsNoAllocation: /health is probed every few seconds by every
// orchestrator and every public-prefix request takes the same route, so the check
// must not add garbage for a canonical path.
func TestCanonicalBypassCostsNoAllocation(t *testing.T) {
	prefixes := newPublicPrefixes([]string{"/c/"})
	for _, target := range []string{"/health", "/openapi.json", "/c/tok", "/c/tok/id", "/c/", "/admin"} {
		u, err := url.ParseRequestURI(target)
		if err != nil {
			t.Fatal(err)
		}
		if n := testing.AllocsPerRun(100, func() { classifyPublicPath(u, prefixes) }); n != 0 {
			t.Errorf("classifyPublicPath(%q) allocates %v times per call, want 0", target, n)
		}
	}
}

// FuzzPublicPathBypass states what "canonical" is for. Whatever request target the
// gate lets through as exempt, no reading of it that a router might apply leaves
// the route the gate saw: the decoded path and the raw path (neither has a dot
// segment, an empty segment or an encoded separator), path.Clean of either, one
// more decoding of the decoded path, and backslashes read as slashes all stay
// under the declared prefix (or, for the bare prefix "/c/", are "/c": a router
// may drop one trailing slash). A fixed path is the same text in both forms. Go's
// fuzzer mutates the seeds (the review's four paths among them); a plain test run
// replays them.
func FuzzPublicPathBypass(f *testing.F) {
	for _, s := range append([]string{"/c/tok", "/c/", "/c/tok/id", "/health", "/c//x", "/c/a%20b", "/c/%41", "/c/x\x00y"}, nonCanonicalProbes...) {
		f.Add(s)
	}
	prefixes := newPublicPrefixes([]string{"/c/"})
	f.Fuzz(func(t *testing.T, target string) {
		u, err := url.ParseRequestURI(target)
		if err != nil || classifyPublicPath(u, prefixes) != pathPublic {
			return
		}
		decoded, raw := u.Path, u.EscapedPath()
		if isFixedPublicPath(decoded) {
			if raw != decoded {
				t.Fatalf("%q was let through as the fixed path %q but its raw form is %q", target, decoded, raw)
			}
			return
		}
		stays := func(read string) bool { return strings.HasPrefix(read, "/c/") || read == "/c" }
		reads := map[string]string{
			"decoded":                   decoded,
			"raw":                       raw,
			"Clean(decoded)":            path.Clean(decoded),
			"Clean(raw)":                path.Clean(raw),
			"Clean(backslash as slash)": path.Clean(strings.ReplaceAll(decoded, "\\", "/")),
		}
		if twice, err := url.PathUnescape(decoded); err == nil {
			reads["decoded twice"] = twice
			reads["Clean(decoded twice)"] = path.Clean(twice)
		}
		for name, read := range reads {
			if !stays(read) {
				t.Fatalf("%q was let through, but a router that reads it as %s gets %q, outside /c/ (decoded %q, raw %q)", target, name, read, decoded, raw)
			}
		}
		for _, form := range []string{decoded, raw} {
			for _, seg := range strings.Split(form, "/")[1:] {
				if seg == "." || seg == ".." {
					t.Fatalf("%q was let through with the dot segment %q in %q", target, seg, form)
				}
			}
			if strings.Contains(form, "//") {
				t.Fatalf("%q was let through with an empty segment in %q", target, form)
			}
		}
		if hasEncodedSeparator(decoded) || hasEncodedSeparator(raw) || strings.Contains(decoded, "\\") {
			t.Fatalf("%q was let through with an encoded or backslash separator (decoded %q, raw %q)", target, decoded, raw)
		}
	})
}
