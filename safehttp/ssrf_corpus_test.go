package safehttp_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
	"github.com/baditaflorin/go-common/testhelpers/ssrfcorpus"
)

func corpusCtx(t *testing.T, c ssrfcorpus.Case) context.Context {
	t.Helper()
	d := 3 * time.Second
	if c.NeedsResolver {
		d = time.Second // a resolver-only name must not stall the suite offline
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// A gap is logged, not asserted: if the guard starts refusing it, the log says
// the Gap marker in ssrfcorpus can go.
func logGap(t *testing.T, c ssrfcorpus.Case, refused bool) {
	t.Helper()
	if refused {
		t.Logf("known gap now closed (%s): remove the Gap marker from ssrfcorpus", c.Gap)
		return
	}
	t.Logf("known gap still open (%s): %s is allowed", c.Gap, c.Host)
}

func TestSSRFCorpusGuardHost(t *testing.T) {
	safehttp.SetAllowedPrivateIPs(nil)
	for _, c := range ssrfcorpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			err := safehttp.GuardHost(corpusCtx(t, c), c.Host)
			switch {
			case c.Gap != "":
				logGap(t, c, err != nil)
			case c.Blocked && err == nil:
				t.Fatalf("GuardHost(%q) allowed a target that must be refused", c.Host)
			case !c.Blocked && err != nil:
				t.Fatalf("GuardHost(%q) refused a public address: %v", c.Host, err)
			}
		})
	}
}

func TestSSRFCorpusIsBlocked(t *testing.T) {
	safehttp.SetAllowedPrivateIPs(nil)
	for _, c := range ssrfcorpus.Cases {
		if !c.IsLiteralIP() {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			got := safehttp.IsBlocked(net.ParseIP(c.Host))
			switch {
			case c.Gap != "":
				logGap(t, c, got)
			case got != c.Blocked:
				t.Fatalf("IsBlocked(%s) = %v, want %v", c.Host, got, c.Blocked)
			}
		})
	}
}

// CheckURL is the one-call guard handlers use: parse, validate, guard.
func TestSSRFCorpusCheckURL(t *testing.T) {
	safehttp.SetAllowedPrivateIPs(nil)
	for _, c := range ssrfcorpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			_, err := safehttp.CheckURL(corpusCtx(t, c), c.URL())
			switch {
			case c.Gap != "":
				logGap(t, c, err != nil)
			case c.Blocked && err == nil:
				t.Fatalf("CheckURL(%q) allowed a target that must be refused", c.URL())
			case !c.Blocked && err != nil:
				t.Fatalf("CheckURL(%q) refused a public address: %v", c.URL(), err)
			}
		})
	}
}

// The client itself must refuse at dial time, before any connection. Only
// literal blocked IPs are tried: they are refused without touching the network.
func TestSSRFCorpusClientRefusesAtDial(t *testing.T) {
	safehttp.SetAllowedPrivateIPs(nil)
	client := safehttp.NewClient(safehttp.WithTimeout(2*time.Second), safehttp.WithoutProxy())
	for _, c := range ssrfcorpus.Cases {
		if !c.Blocked || !c.IsLiteralIP() || c.Gap != "" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			resp, err := client.Get(c.URL())
			if err == nil {
				_ = resp.Body.Close()
				t.Fatalf("GET %s succeeded; the client must refuse it", c.URL())
			}
			if !errors.Is(err, safehttp.ErrBlocked) {
				t.Fatalf("GET %s failed, but not with ErrBlocked: %v", c.URL(), err)
			}
		})
	}
}
