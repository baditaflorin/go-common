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
