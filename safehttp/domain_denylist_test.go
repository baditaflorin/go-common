package safehttp

import (
	"context"
	"errors"
	"testing"
)

func TestIsDomainDenied(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"sci-hub.st", true},
		{"sci-hub.ru", true},
		{"thepiratebay.org", true},
		{"thepiratebay.se", true},
		{"rapidgator.net", true},
		{"avito.st", true},
		{"avito.ru", true},
		{"avitop.com", true},
		{"rutracker.org", true},
		{"ddos-guard.net", true},
		{"vglista.no", true},
		// case-insensitive
		{"RuTracker.ORG", true},
		// trailing-dot FQDN
		{"rutracker.org.", true},
		// subdomain
		{"www.rutracker.org", true},
		{"static.cdn.ddos-guard.net", true},
		// host:port
		{"rutracker.org:443", true},
		// not denied
		{"example.com", false},
		{"rutracker.org.evil.com", false}, // suffix must be on a label boundary
		{"notrutracker.org", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsDomainDenied(c.host); got != c.want {
			t.Errorf("IsDomainDenied(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestGuardHost_DomainDenylist(t *testing.T) {
	err := GuardHost(context.Background(), "rutracker.org")
	if !errors.Is(err, ErrDomainDenied) {
		t.Fatalf("GuardHost(rutracker.org) = %v, want ErrDomainDenied", err)
	}
}

func TestCheckURL_DomainDenylist(t *testing.T) {
	_, err := CheckURL(context.Background(), "https://sci-hub.st/some/paper")
	if !errors.Is(err, ErrDomainDenied) {
		t.Fatalf("CheckURL(sci-hub.st) = %v, want ErrDomainDenied", err)
	}
}

func TestDeniedDomains_Sorted(t *testing.T) {
	list := DeniedDomains()
	if len(list) != len(deniedDomains) {
		t.Fatalf("DeniedDomains() returned %d entries, want %d", len(list), len(deniedDomains))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1] >= list[i] {
			t.Fatalf("DeniedDomains() not sorted: %v", list)
		}
	}
}
