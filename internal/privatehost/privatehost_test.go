package privatehost

import "testing"

func TestName(t *testing.T) {
	for host, want := range map[string]bool{
		"go_wps-app-1":              true,
		"go-fleet-graph":            true,
		"localhost":                 true,
		"GO_WPS-APP-1.":             true,
		"db.internal":               true,
		"printer.local":             true,
		"api.svc":                   true,
		"host.docker.internal":      true,
		"nas.home.arpa":             true,
		"":                          false,
		"example.com":               false,
		"api.0crawl.com":            false,
		"internal.example.com":      false,
		"notlocal.com":              false,
		"10.0.0.5":                  false,
		"8.8.8.8":                   false,
		"::1":                       false,
		"[2001:db8::1]":             false,
		"p.webshare.io":             false,
		"google.com":                false,
		"localdomain-looking.co.uk": false,
	} {
		if got := Name(host); got != want {
			t.Errorf("Name(%q) = %v, want %v", host, got, want)
		}
	}
}
