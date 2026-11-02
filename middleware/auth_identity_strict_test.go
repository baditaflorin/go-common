package middleware

import (
	"net/http"
	"testing"
)

// strictFakeIdentity is a fakeIdentity that also implements StrictIdentity.
type strictFakeIdentity struct {
	fakeIdentity
	strict bool
}

func (s strictFakeIdentity) StrictPresented() bool { return s.strict }

func presentedUnverified(*http.Request) IdentityDecision {
	return IdentityDecision{Presented: true, Source: AuthSourceSPIFFEJWT, Reason: "bad_signature"}
}

// A presented JWT-SVID that does not verify, arriving from a private address with no key:
// default dual mode lets TrustPrivateMesh admit it (unchanged); strict mode refuses it.
func TestStrictIdentity_PrivateMeshDoesNotRescueAnUnverifiedPresentedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		strict bool
		want   int
	}{
		{"default dual mode: private mesh admits (unchanged)", false, 200},
		{"strict: refused 401", true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			id := strictFakeIdentity{fakeIdentity{decide: presentedUnverified}, tc.strict}
			mw := TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(map[string]string{"ak_x": "svc-a"}, &seen), Identity: id, TrustPrivateMesh: true})
			r := newReq("/x")
			r.RemoteAddr = "10.10.10.10:4444"
			r.Header.Set("X-Spiffe-Jwt", "forged")
			if rr, _ := serve(mw, r); rr.Code != tc.want {
				t.Fatalf("code=%d want %d", rr.Code, tc.want)
			}
		})
	}
}

// Strict mode changes nothing for a request that presents nothing, nor for one that also carries a valid key.
func TestStrictIdentity_NoIdentityAndValidKeyUnchanged(t *testing.T) {
	var seen []string
	none := strictFakeIdentity{fakeIdentity{decide: func(*http.Request) IdentityDecision { return IdentityDecision{} }}, true}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(map[string]string{"ak_x": "svc-a"}, &seen), Identity: none, TrustPrivateMesh: true})
	r := newReq("/x")
	r.RemoteAddr = "10.10.10.10:4444"
	if rr, _ := serve(mw, r); rr.Code != 200 {
		t.Fatalf("no identity from a private peer must still be admitted by the mesh, code=%d", rr.Code)
	}
	bad := strictFakeIdentity{fakeIdentity{decide: presentedUnverified}, true}
	mw = TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(map[string]string{"ak_x": "svc-a"}, &seen), Identity: bad, TrustPrivateMesh: true})
	r = newReq("/x")
	r.RemoteAddr = "10.10.10.10:4444"
	r.Header.Set("X-API-Key", "ak_x")
	if rr, _ := serve(mw, r); rr.Code != 200 {
		t.Fatalf("a valid key still authenticates next to a bad token, code=%d", rr.Code)
	}
}
