package proxysupplier

import "testing"

// With no NO_PROXY at all, names that can only be internal still bypass the proxy.
func TestBypass_InternalNamesWithoutRules(t *testing.T) {
	suppliers := map[string]interface{ Bypass(string) bool }{
		"url":   &urlSupplier{name: "t", rawURL: "http://p:80"},
		"multi": &multiSupplier{},
	}
	for name, s := range suppliers {
		for host, want := range map[string]bool{
			"go_wps-app-1": true, "go-fleet-graph": true, "db.internal": true,
			"example.com": false, "8.8.8.8": false, "": false,
		} {
			if got := s.Bypass(host); got != want {
				t.Errorf("%s.Bypass(%q) = %v, want %v", name, host, got, want)
			}
		}
	}
}
