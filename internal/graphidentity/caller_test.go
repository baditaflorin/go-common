package graphidentity

import (
	"context"
	"testing"
)

func TestWithVerifiedPrincipalAcceptsFleetServiceIDsOnly(t *testing.T) {
	cases := map[string]string{
		"go_apikey_scanner":    "go_apikey_scanner",
		"go-pentest-subfinder": "go-pentest-subfinder",
		"fleet-runner":         "fleet-runner",
		"internal":             "",
		"operator":             "",
		"Mozilla/5.0":          "",
		"curl/7.88.1":          "",
		"":                     "",
		"randomthing":          "",
		"Go-service":           "",
		"go-service/1.0":       "",
	}
	for principal, want := range cases {
		ctx := WithVerifiedPrincipal(context.Background(), principal)
		if got := VerifiedPrincipal(ctx); got != want {
			t.Errorf("VerifiedPrincipal(%q) = %q; want %q", principal, got, want)
		}
	}
}
