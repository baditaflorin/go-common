package egressproxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
	"github.com/baditaflorin/go-common/testhelpers/ssrfcorpus"
)

// DefaultGuard is what stands between a browser and the internal network, so it
// gets the shared corpus too. A literal blocked address is a denial; a name it
// cannot verify is "unavailable"; both refuse the CONNECT.
func TestDefaultGuardSSRFCorpus(t *testing.T) {
	safehttp.SetAllowedPrivateIPs(nil)
	for _, c := range ssrfcorpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			d := 3 * time.Second
			if c.NeedsResolver {
				d = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), d)
			defer cancel()
			err := DefaultGuard(ctx, c.Host, "443")
			switch {
			case c.Gap != "":
				if err != nil {
					t.Logf("known gap now closed (%s): remove the Gap marker from ssrfcorpus", c.Gap)
				} else {
					t.Logf("known gap still open (%s): %s is allowed", c.Gap, c.Host)
				}
			case c.Blocked && err == nil:
				t.Fatalf("DefaultGuard(%q) allowed a target that must be refused", c.Host)
			case c.Blocked && !c.NeedsResolver && !errors.Is(err, ErrDenied):
				t.Fatalf("DefaultGuard(%q) = %v, want ErrDenied for a literal blocked address", c.Host, err)
			case !c.Blocked && err != nil:
				t.Fatalf("DefaultGuard(%q) refused a public address: %v", c.Host, err)
			}
		})
	}
}
