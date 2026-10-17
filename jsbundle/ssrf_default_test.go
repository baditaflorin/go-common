package jsbundle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
	"github.com/baditaflorin/go-common/testhelpers/ssrfcorpus"
)

// With no Client set, Recover must not be steerable at internal addresses: the
// URLs it fetches come from pages and error reports.
func TestRecoverDefaultClientRefusesInternalTargets(t *testing.T) {
	safehttp.SetAllowedPrivateIPs(nil)
	for _, c := range ssrfcorpus.Cases {
		if !c.Blocked || !c.IsLiteralIP() || c.Gap != "" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := Recover(ctx, c.URL()+"app.js", RecoverOptions{})
			if err == nil {
				t.Fatalf("Recover fetched %s with the default client", c.URL())
			}
			if !errors.Is(err, safehttp.ErrBlocked) {
				t.Fatalf("Recover(%s) failed, but not with ErrBlocked: %v", c.URL(), err)
			}
		})
	}
}

func TestDefaultClientIsSharedAndHasATimeout(t *testing.T) {
	a, errA := guardedDefaultClient()
	b, errB := guardedDefaultClient()
	if errA != nil || errB != nil {
		t.Fatalf("guardedDefaultClient() errors: %v, %v", errA, errB)
	}
	if a != b {
		t.Fatal("the default client should be built once and shared")
	}
	if a.Timeout != defaultFetchTimeout {
		t.Fatalf("default client timeout = %v, want %v", a.Timeout, defaultFetchTimeout)
	}
}
