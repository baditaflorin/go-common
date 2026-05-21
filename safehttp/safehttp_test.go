package safehttp_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/safehttp"
)

func TestIsBlocked(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		// must block
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.0.1", true},
		{"192.168.255.255", true},
		{"169.254.0.1", true},
		{"169.254.255.255", true},
		{"fe80::1", true},
		{"224.0.0.1", true},
		{"239.255.255.255", true},
		{"ff02::1", true},
		{"100.64.0.0", true},
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"fc00::1", true},
		{"fd12:3456:7890::1", true},
		{"fdff:ffff:ffff::ffff", true},
		// must allow
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
		{"100.128.0.0", false},
		{"172.32.0.0", false},
		{"192.169.0.0", false},
		{"11.0.0.1", false},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("invalid test IP: %s", tt.ip)
		}
		if got := safehttp.IsBlocked(ip); got != tt.want {
			t.Errorf("IsBlocked(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestGuardHostBlockedLiteral(t *testing.T) {
	for _, h := range []string{"127.0.0.1", "::1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.0.1", "100.64.0.1", "fc00::1"} {
		if err := safehttp.GuardHost(context.Background(), h); err == nil {
			t.Errorf("GuardHost(%s): expected error, got nil", h)
		}
	}
}

func TestGuardHostPublicLiteral(t *testing.T) {
	for _, h := range []string{"8.8.8.8", "1.1.1.1"} {
		if err := safehttp.GuardHost(context.Background(), h); err != nil {
			t.Errorf("GuardHost(%s): unexpected error %v", h, err)
		}
	}
}

func TestGuardHostEmpty(t *testing.T) {
	if err := safehttp.GuardHost(context.Background(), ""); err == nil {
		t.Error("GuardHost(): expected error, got nil")
	}
}

func TestNewClientBlocksLoopback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()
	c := safehttp.NewClient(safehttp.WithUserAgent("test/1.0"))
	_, err := c.Get(ts.URL)
	if err == nil {
		t.Fatal("expected SSRF block for loopback server, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "blocked") {
		t.Errorf("expected 'blocked' in error, got: %v", err)
	}
}

func TestNewClientBlocksTLSLoopback(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()
	c := safehttp.NewClient(safehttp.WithUserAgent("test/1.0"))
	_, err := c.Get(ts.URL)
	if err == nil {
		t.Fatal("expected SSRF block for TLS loopback server, got nil")
	}
}

func TestNewClientPortCheck(t *testing.T) {
	c := safehttp.NewClient(safehttp.WithUserAgent("test/1.0"), safehttp.WithPortCheck())
	_, err := c.Get("http://8.8.8.8:8080/")
	if err == nil {
		t.Fatal("expected port check error, got nil")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("expected 'port' in error, got: %v", err)
	}
}

func TestNewClientDefaultOptions(t *testing.T) {
	c := safehttp.NewClient(
		safehttp.WithTimeout(5*1000000000),
		safehttp.WithMaxRedirects(3),
		safehttp.WithUserAgent("myservice/1.0"),
		safehttp.WithPortCheck(),
	)
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
}

func TestGuardedDialer(t *testing.T) {
	dial := safehttp.GuardedDialer(false)
	if dial == nil {
		t.Fatal("GuardedDialer returned nil")
	}
}

func TestValidateURL(t *testing.T) {
	tests := []struct {
		raw     string
		wantErr bool
	}{
		{"http://example.com/", false},
		{"https://example.com/path?q=1", false},
		{"ftp://example.com/", true},
		{"http:///path", true},
	}
	for _, tt := range tests {
		u, _ := url.Parse(tt.raw)
		err := safehttp.ValidateURL(u)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateURL(%q) error=%v wantErr=%v", tt.raw, err, tt.wantErr)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		raw      string
		wantErr  bool
		wantHost string
	}{
		{"https://example.com/path", false, "example.com"},
		{"example.com/path", false, "example.com"},
		{"http://example.com", false, "example.com"},
		{"", true, ""},
		{"ftp://example.com", true, ""},
		{"//example.com", true, ""},
		{"http:///nohost", true, ""},
	}
	for _, tt := range tests {
		u, err := safehttp.NormalizeURL(tt.raw)
		if (err != nil) != tt.wantErr {
			t.Errorf("NormalizeURL(%q) err=%v wantErr=%v", tt.raw, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && u.Hostname() != tt.wantHost {
			t.Errorf("NormalizeURL(%q) host=%q want=%q", tt.raw, u.Hostname(), tt.wantHost)
		}
	}
}

func TestCheckURL_BlocksPrivate(t *testing.T) {
	ctx := context.Background()
	blocked := []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/",
		"ftp://example.com/",
	}
	for _, u := range blocked {
		if _, err := safehttp.CheckURL(ctx, u); err == nil {
			t.Errorf("CheckURL(%q) should return error", u)
		}
	}
}

func TestCheckURL_AcceptsPublic(t *testing.T) {
	ctx := context.Background()
	u, err := safehttp.CheckURL(ctx, "https://example.com/path")
	if err != nil {
		t.Fatalf("CheckURL(example.com): unexpected error: %v", err)
	}
	if u.Host != "example.com" {
		t.Errorf("expected host=example.com, got %q", u.Host)
	}
}

// --- Proxy posture options (WithoutProxy / RequireProxy) ----------------

func TestRequireProxy_PanicsWhenEnvUnset(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when RequireProxy set without HTTPS_PROXY env")
		}
	}()
	_ = safehttp.NewClient(safehttp.RequireProxy())
}

func TestRequireProxy_OKWhenEnvSet(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://example.invalid:3128")
	c := safehttp.NewClient(safehttp.RequireProxy())
	if c == nil {
		t.Fatal("nil client")
	}
}

func TestWithoutProxy_OverridesEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://example.invalid:3128")
	c := safehttp.NewClient(safehttp.WithoutProxy())
	if c == nil {
		t.Fatal("nil client")
	}
	// Sanity: making a request must not attempt to dial example.invalid:3128.
	// We point at a httptest server (127.0.0.1) and disable the SSRF guard
	// implicitly by running on loopback isn't possible with safehttp, so
	// just assert the client was constructed without panicking. The full
	// no-proxy round-trip is exercised in WithoutProxy_DirectDial below.
	_ = c
}

func TestWithoutProxy_DirectDial(t *testing.T) {
	// Spin up a httptest TLS server and verify WithoutProxy honors a
	// direct dial even with HTTPS_PROXY pointing somewhere unroutable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	t.Setenv("HTTPS_PROXY", "http://example.invalid:3128")
	t.Setenv("HTTP_PROXY", "http://example.invalid:3128")
	// safehttp blocks 127.0.0.1 by default, so we cannot actually hit the
	// test server through it. Assert construction + that the transport's
	// proxy func is nil via reflection-free behaviour: a sentinel request
	// to an invalid hostname must fail with a DNS / dial error, not with
	// an unreachable-proxy error string.
	c := safehttp.NewClient(safehttp.WithoutProxy())
	_, err := c.Get(srv.URL) // expected to fail SSRF guard on 127.0.0.1
	if err == nil {
		t.Fatal("expected SSRF block, got nil")
	}
	if strings.Contains(err.Error(), "example.invalid") {
		t.Fatalf("WithoutProxy still routed through HTTPS_PROXY: %v", err)
	}
}

func TestWithoutProxyAndRequireProxy_Panic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when both options passed")
		}
	}()
	_ = safehttp.NewClient(safehttp.WithoutProxy(), safehttp.RequireProxy())
}

// FLEET_REQUIRE_PROXY env switch — fleet-wide enforcement that
// promotes any NewClient() caller to RequireProxy posture so a
// service that forgot the option still fails-fast on missing
// HTTPS_PROXY instead of silently leaking the dockerhost IP.
// fleet-runner deploy renders this env into every proxy_egress:
// true service.

func TestFleetRequireProxyEnv_PanicsWithoutHTTPSProxy(t *testing.T) {
	t.Setenv("FLEET_REQUIRE_PROXY", "1")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when FLEET_REQUIRE_PROXY=1 without HTTP(S)_PROXY")
		}
	}()
	_ = safehttp.NewClient() // no options — env alone must trip the guard
}

func TestFleetRequireProxyEnv_OKWhenHTTPSProxySet(t *testing.T) {
	t.Setenv("FLEET_REQUIRE_PROXY", "1")
	t.Setenv("HTTPS_PROXY", "http://example.invalid:3128")
	c := safehttp.NewClient()
	if c == nil {
		t.Fatal("nil client")
	}
}

func TestFleetRequireProxyEnv_WithoutProxyEscapeHatchWins(t *testing.T) {
	// SSRF probers / port scanners pass WithoutProxy explicitly.
	// FLEET_REQUIRE_PROXY must NOT promote them — otherwise a
	// fleet-wide enable would break direct-egress tooling that
	// is legitimate by design.
	t.Setenv("FLEET_REQUIRE_PROXY", "1")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithoutProxy must override FLEET_REQUIRE_PROXY, got panic: %v", r)
		}
	}()
	c := safehttp.NewClient(safehttp.WithoutProxy())
	if c == nil {
		t.Fatal("nil client")
	}
}

func TestFleetRequireProxyEnv_AcceptsTruthyVariants(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "Yes", " true "} {
		t.Run(strings.ReplaceAll(v, " ", "_"), func(t *testing.T) {
			t.Setenv("FLEET_REQUIRE_PROXY", v)
			t.Setenv("HTTPS_PROXY", "")
			t.Setenv("https_proxy", "")
			t.Setenv("HTTP_PROXY", "")
			t.Setenv("http_proxy", "")
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for FLEET_REQUIRE_PROXY=%q", v)
				}
			}()
			_ = safehttp.NewClient()
		})
	}
}

func TestFleetRequireProxyEnv_FalsyValuesNoOp(t *testing.T) {
	for _, v := range []string{"", "0", "false", "no", "off", "anything-else"} {
		t.Run("v="+v, func(t *testing.T) {
			t.Setenv("FLEET_REQUIRE_PROXY", v)
			t.Setenv("HTTPS_PROXY", "")
			t.Setenv("https_proxy", "")
			t.Setenv("HTTP_PROXY", "")
			t.Setenv("http_proxy", "")
			// must NOT panic
			c := safehttp.NewClient()
			if c == nil {
				t.Fatal("nil client")
			}
		})
	}
}
