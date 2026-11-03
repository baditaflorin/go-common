package middleware

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/header"
)

type xauthRec struct {
	mu   sync.Mutex
	seen [][4]string
	user string // X-Auth-User the downstream handler saw on the last admitted request
}

func (x *xauthRec) ObserveXAuthUntrusted(mode, src, caller, cred string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.seen = append(x.seen, [4]string{mode, src, caller, cred})
}
func (x *xauthRec) ObserveAuth(AuthEvent) {}

func xauthKeystore(t *testing.T, mode, cidrs string, opts KeystoreOpts) (http.Handler, *stubVerifier, *xauthRec) {
	t.Helper()
	t.Setenv(EnvXAuthTrustMode, mode)
	t.Setenv(EnvXAuthTrustedCIDRs, cidrs)
	v := &stubVerifier{verify: func(_ context.Context, key string) (*apikey.VerifyResult, error) {
		if key != "ak_good" {
			return nil, apikey.ErrInvalidKey
		}
		return &apikey.VerifyResult{User: "real", Scope: "s", Tier: "t"}, nil
	}}
	rec := &xauthRec{}
	opts.Verifier = v
	opts.Observer = rec
	opts.Logger = log.New(io.Discard, "", 0)
	h := TokenAuthKeystore(opts)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.user = r.Header.Get(header.AuthUser)
		w.WriteHeader(http.StatusOK)
	}))
	return h, v, rec
}

func xauthReq(peer string, mutate func(*http.Request)) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/work", nil)
	r.RemoteAddr = peer
	r.Header.Set(header.AuthUser, "forged")
	r.Header.Set("X-Fleet-Caller", "probe")
	if mutate != nil {
		mutate(r)
	}
	return r
}

func xaServe(h http.Handler, r *http.Request) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// Off (the default) is the legacy contract: the header is trusted from any peer, nothing is counted.
func TestXAuth_OffIsLegacy(t *testing.T) {
	for _, mode := range []string{"", "off", "bogus"} {
		h, v, rec := xauthKeystore(t, mode, "", KeystoreOpts{})
		if c := xaServe(h, xauthReq("10.10.10.77:5000", nil)); c != http.StatusOK {
			t.Fatalf("mode %q: forged header from a direct peer = %d, want 200 (legacy)", mode, c)
		}
		if v.calls != 0 || len(rec.seen) != 0 {
			t.Fatalf("mode %q: verifier calls %d, observations %d; want 0, 0", mode, v.calls, len(rec.seen))
		}
	}
}

// Shadow admits exactly like off and counts the untrusted source, by what else the request carried.
func TestXAuth_ShadowAdmitsAndCounts(t *testing.T) {
	h, _, rec := xauthKeystore(t, "shadow", "", KeystoreOpts{})
	if c := xaServe(h, xauthReq("10.10.10.77:5000", nil)); c != http.StatusOK {
		t.Fatalf("shadow, forged, no credential = %d, want 200 (admitted, as today)", c)
	}
	if c := xaServe(h, xauthReq("[::1]:5000", func(r *http.Request) { r.Header.Set(header.APIKey, "ak_good") })); c != http.StatusOK {
		t.Fatalf("shadow, forged + key = %d", c)
	}
	if c := xaServe(h, xauthReq("10.10.10.77:5000", func(r *http.Request) { r.Header.Set("X-Spiffe-Jwt", "x.y.z") })); c != http.StatusOK {
		t.Fatalf("shadow, forged + jwt header = %d", c)
	}
	// the default trusted address is the gateway: not counted
	if c := xaServe(h, xauthReq("10.10.10.10:5000", nil)); c != http.StatusOK {
		t.Fatalf("shadow, gateway = %d", c)
	}
	// public paths never reach the header check
	pr := xauthReq("10.10.10.77:5000", nil)
	pr.URL.Path = "/health"
	xaServe(h, pr)
	want := [][4]string{
		{"shadow", "10.10.10.77", "probe", "none"},
		{"shadow", "::1", "probe", "key"},
		{"shadow", "10.10.10.77", "probe", "jwt"},
	}
	if len(rec.seen) != len(want) {
		t.Fatalf("observations = %v, want %v", rec.seen, want)
	}
	for i := range want {
		if rec.seen[i] != want[i] {
			t.Fatalf("observation %d = %v, want %v", i, rec.seen[i], want[i])
		}
	}
}

// Enforce: X-Auth-* is trusted only from the list; a direct peer needs a real credential.
func TestXAuth_Enforce(t *testing.T) {
	h, v, rec := xauthKeystore(t, "enforce", "", KeystoreOpts{})
	if c := xaServe(h, xauthReq("10.10.10.77:5000", nil)); c != http.StatusUnauthorized {
		t.Fatalf("enforce, forged from a direct peer, no credential = %d, want 401", c)
	}
	if c := xaServe(h, xauthReq("10.10.10.10:5000", nil)); c != http.StatusOK {
		t.Fatalf("enforce, from the gateway = %d, want 200", c)
	}
	// a valid key still works from a direct peer: verified by the keystore, the forged user is replaced by the real one
	if c := xaServe(h, xauthReq("10.10.10.77:5000", func(r *http.Request) { r.Header.Set(header.APIKey, "ak_good") })); c != http.StatusOK || rec.user != "real" || v.calls != 1 {
		t.Fatalf("enforce, forged + valid key: code %d user %q calls %d, want 200, the verified user, one keystore call", c, rec.user, v.calls)
	}
	if c := xaServe(h, xauthReq("10.10.10.77:5000", func(r *http.Request) { r.Header.Set(header.APIKey, "ak_bad") })); c != http.StatusUnauthorized {
		t.Fatalf("enforce, forged + bad key = %d, want 401", c)
	}
	if v.calls != 2 {
		t.Fatalf("verifier calls = %d, want 2 (the good and the bad key)", v.calls)
	}
	if len(rec.seen) != 3 || rec.seen[0][0] != "enforce" || rec.seen[0][3] != "none" || rec.seen[1][3] != "key" {
		t.Fatalf("observations = %v", rec.seen)
	}
}

// A custom list, a bare address, a bad entry; and a list that parses to nothing never means "trust everyone".
func TestXAuth_TrustedList(t *testing.T) {
	h, _, _ := xauthKeystore(t, "enforce", "10.20.0.0/16, 192.168.1.5, junk", KeystoreOpts{})
	for peer, want := range map[string]int{"10.20.3.4:1": 200, "192.168.1.5:1": 200, "10.10.10.10:1": 401, "192.168.1.6:1": 401} {
		if c := xaServe(h, xauthReq(peer, nil)); c != want {
			t.Fatalf("peer %s = %d, want %d", peer, c, want)
		}
	}
	h, _, _ = xauthKeystore(t, "enforce", "junk,also junk", KeystoreOpts{})
	if c := xaServe(h, xauthReq("10.10.10.77:1", nil)); c != http.StatusUnauthorized {
		t.Fatalf("all-invalid list = %d, want 401 (falls back to the gateway address)", c)
	}
	if c := xaServe(h, xauthReq("10.10.10.10:1", nil)); c != http.StatusOK {
		t.Fatalf("all-invalid list, gateway = %d, want 200", c)
	}
}

// A service's own trusted-proxy list wins; the variables are ignored.
func TestXAuth_ExplicitListWins(t *testing.T) {
	h, _, rec := xauthKeystore(t, "shadow", "10.99.0.0/16", KeystoreOpts{TrustedGatewayCIDRs: []string{"10.10.10.10/32"}})
	if c := xaServe(h, xauthReq("10.99.0.5:1", nil)); c != http.StatusUnauthorized {
		t.Fatalf("explicit list, peer outside it = %d, want 401 (the service's list, not XAUTH_TRUSTED_CIDRS)", c)
	}
	if len(rec.seen) != 0 {
		t.Fatalf("explicit list: observations %v, want none (the variables are ignored)", rec.seen)
	}
}
