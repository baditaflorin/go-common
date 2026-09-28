package graph

import (
	"net/http/httptest"
	"testing"
)

func TestParseTrustedCallerIPsAcceptsLiteralAddressesOnly(t *testing.T) {
	got := parseTrustedCallerIPs(" 10.10.10.10, ::ffff:10.10.10.11, 10.0.0.0/8, invalid ")
	if len(got) != 2 {
		t.Fatalf("got %d trusted IPs, want 2", len(got))
	}
	if got[0].String() != "10.10.10.10" || got[1].String() != "10.10.10.11" {
		t.Fatalf("unexpected normalized trusted IPs: %v", got)
	}
}

func TestTrustedGatewayCallerRequiresExactPeerAndValidServiceID(t *testing.T) {
	trusted := parseTrustedCallerIPs("10.10.10.10")

	request := httptest.NewRequest("GET", "http://proxy/", nil)
	request.RemoteAddr = "10.10.10.10:45678"
	request.Header.Set("X-Auth-User", "go_search_duck")
	if got := trustedGatewayCaller(request, trusted); got != "go_search_duck" {
		t.Fatalf("trusted gateway caller = %q, want go_search_duck", got)
	}

	request.RemoteAddr = "10.10.10.11:45678"
	if got := trustedGatewayCaller(request, trusted); got != "" {
		t.Fatalf("untrusted peer caller = %q, want empty", got)
	}

	request.RemoteAddr = "10.10.10.10:45678"
	request.Header.Set("X-Auth-User", "Browser/User")
	if got := trustedGatewayCaller(request, trusted); got != "" {
		t.Fatalf("invalid caller label = %q, want empty", got)
	}
}
