package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/internal/graphidentity"
)

type fakeIdentity struct {
	decide   func(*http.Request) IdentityDecision
	expected map[string]string
}

func (f fakeIdentity) AuthenticateIdentity(r *http.Request) IdentityDecision { return f.decide(r) }
func (f fakeIdentity) ExpectedPrincipal(u string) (string, bool) {
	p, ok := f.expected[u]
	return p, ok
}

type recObs struct {
	mu        sync.Mutex
	events    []AuthEvent
	failures  []string
	fallbacks []string
}

func (o *recObs) ObserveAuth(e AuthEvent) { o.mu.Lock(); o.events = append(o.events, e); o.mu.Unlock() }
func (o *recObs) ObserveIdentityFailure(s AuthSource, reason string) {
	o.mu.Lock()
	o.failures = append(o.failures, string(s)+"/"+reason)
	o.mu.Unlock()
}
func (o *recObs) ObserveIdentityFallback(p, reason string) {
	o.mu.Lock()
	o.fallbacks = append(o.fallbacks, p+"/"+reason)
	o.mu.Unlock()
}
func (o *recObs) last() AuthEvent { return o.events[len(o.events)-1] }

func keyVerifier(tokens map[string]string, seen *[]string) *stubVerifier {
	return &stubVerifier{verify: func(_ context.Context, key string) (*apikey.VerifyResult, error) {
		*seen = append(*seen, key)
		if u, ok := tokens[key]; ok {
			return &apikey.VerifyResult{User: u, Scope: "read", Tier: "free"}, nil
		}
		return nil, apikey.ErrInvalidKey
	}}
}

func serve(mw Middleware, r *http.Request) (*httptest.ResponseRecorder, *http.Request) {
	var got *http.Request
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) { got = rq; w.WriteHeader(200) }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr, got
}

func TestIdentity_NilIsUnchanged(t *testing.T) {
	var seen []string
	obs := &recObs{}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(map[string]string{"ak_x": "svc-a"}, &seen), Observer: obs})
	r := newReq("/x")
	r.Header.Set("Authorization", "Bearer ak_x")
	if rr, _ := serve(mw, r); rr.Code != 200 || obs.last().Source != AuthSourceKeystore {
		t.Fatalf("code=%d source=%s", rr.Code, obs.last().Source)
	}
	if len(obs.fallbacks) != 0 || len(obs.failures) != 0 {
		t.Fatal("no identity events expected when Identity is nil")
	}
}

func TestIdentity_AllowedReplacesForgedHeadersAndSkipsKeystore(t *testing.T) {
	var seen []string
	obs := &recObs{}
	id := fakeIdentity{decide: func(*http.Request) IdentityDecision {
		return IdentityDecision{Presented: true, Allowed: true, Source: AuthSourceSPIFFEMTLS,
			SPIFFEID: "spiffe://0exec.com/ns/fleet/sa/agent-tools", Principal: "agent-tools", Scope: "write", Tier: "internal"}
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(nil, &seen), Observer: obs, Identity: id,
		RequiredTier: "internal", TierEnforce: true})
	r := newReq("/x")
	r.Header.Set(header.AuthUser, "forged")
	r.Header.Set(header.AuthScope, "admin")
	rr, got := serve(mw, r)
	if rr.Code != 200 || len(seen) != 0 {
		t.Fatalf("code=%d keystore calls=%d", rr.Code, len(seen))
	}
	if got.Header.Get(header.AuthUser) != "agent-tools" || got.Header.Get(header.AuthScope) != "write" || got.Header.Get(header.AuthTier) != "internal" {
		t.Fatalf("headers not replaced: %v", got.Header)
	}
	if SPIFFEIDFromContext(got.Context()) != "spiffe://0exec.com/ns/fleet/sa/agent-tools" || graphidentity.VerifiedPrincipal(got.Context()) != "agent-tools" {
		t.Fatal("verified identity not in the context")
	}
	if ev := obs.last(); ev.Source != AuthSourceSPIFFEMTLS || ev.Result != AuthResultAllow {
		t.Fatalf("event = %+v", ev)
	}
}

func TestIdentity_TierGateAppliesToSPIFFE(t *testing.T) {
	id := fakeIdentity{decide: func(*http.Request) IdentityDecision {
		return IdentityDecision{Presented: true, Allowed: true, Source: AuthSourceSPIFFEJWT, Principal: "svc-a", Tier: "free"}
	}}
	obs := &recObs{}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(nil, new([]string)), Observer: obs, Identity: id, RequiredTier: "internal", TierEnforce: true})
	rr, _ := serve(mw, newReq("/x"))
	if rr.Code != http.StatusForbidden || obs.last().Result != AuthResultTierDenied {
		t.Fatalf("code=%d result=%s", rr.Code, obs.last().Result)
	}
}

func TestIdentity_NotPresentedGoesTheKeyPathAndCountsFallback(t *testing.T) {
	var seen []string
	obs := &recObs{}
	id := fakeIdentity{decide: func(*http.Request) IdentityDecision { return IdentityDecision{} }, expected: map[string]string{"svc-a": "svc-a"}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(map[string]string{"ak_x": "svc-a", "ak_y": "other"}, &seen), Observer: obs, Identity: id})
	for _, key := range []string{"ak_x", "ak_y"} {
		r := newReq("/x")
		r.Header.Set(header.APIKey, key)
		if rr, _ := serve(mw, r); rr.Code != 200 {
			t.Fatalf("%s: code=%d", key, rr.Code)
		}
	}
	if len(obs.fallbacks) != 1 || obs.fallbacks[0] != "svc-a/no_svid" {
		t.Fatalf("fallbacks = %v (only the mapped principal counts)", obs.fallbacks)
	}
	// Dual mode: a request with neither identity nor key is still just "missing token".
	rr, _ := serve(mw, newReq("/x"))
	if rr.Code != 401 || obs.last().Source != AuthSourceMissing {
		t.Fatalf("code=%d source=%s", rr.Code, obs.last().Source)
	}
}

func TestIdentity_GatewayPathCountsFallback(t *testing.T) {
	obs := &recObs{}
	id := fakeIdentity{decide: func(*http.Request) IdentityDecision { return IdentityDecision{} }, expected: map[string]string{"svc-a": "svc-a"}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(nil, new([]string)), Observer: obs, Identity: id})
	r := newReq("/x")
	r.Header.Set(header.AuthUser, "svc-a")
	if rr, _ := serve(mw, r); rr.Code != 200 {
		t.Fatalf("code=%d", rr.Code)
	}
	if len(obs.fallbacks) != 1 {
		t.Fatalf("fallbacks = %v", obs.fallbacks)
	}
}

func TestIdentity_PresentedButRejected(t *testing.T) {
	mk := func(d IdentityDecision) (Middleware, *recObs, *[]string) {
		seen := new([]string)
		obs := &recObs{}
		id := fakeIdentity{decide: func(*http.Request) IdentityDecision { return d }, expected: map[string]string{"svc-a": "svc-a"}}
		return TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(map[string]string{"ak_x": "svc-a"}, seen), Observer: obs, Identity: id}), obs, seen
	}

	t.Run("deny: 403, key path not tried", func(t *testing.T) {
		mw, obs, seen := mk(IdentityDecision{Presented: true, Source: AuthSourceSPIFFEMTLS, Reason: "unmapped_id", Deny: true, Status: 403})
		r := newReq("/x")
		r.Header.Set(header.APIKey, "ak_x")
		rr, _ := serve(mw, r)
		if rr.Code != 403 || len(*seen) != 0 || obs.failures[0] != "spiffe-mtls/unmapped_id" || obs.last().Result != AuthResultDeny {
			t.Fatalf("code=%d seen=%v failures=%v", rr.Code, *seen, obs.failures)
		}
	})

	t.Run("soft failure with a key: key path decides, counted as a fallback with the reason", func(t *testing.T) {
		mw, obs, _ := mk(IdentityDecision{Presented: true, Source: AuthSourceSPIFFEJWT, Reason: "expired_token"})
		r := newReq("/x")
		r.Header.Set(header.APIKey, "ak_x")
		rr, _ := serve(mw, r)
		if rr.Code != 200 || obs.failures[0] != "spiffe-jwt/expired_token" || obs.fallbacks[0] != "svc-a/expired_token" {
			t.Fatalf("code=%d failures=%v fallbacks=%v", rr.Code, obs.failures, obs.fallbacks)
		}
	})

	t.Run("soft failure without a key: 401 naming the SPIFFE identity", func(t *testing.T) {
		mw, obs, _ := mk(IdentityDecision{Presented: true, Source: AuthSourceSPIFFEJWT, Reason: "invalid_token", BearerIsSVID: true})
		r := newReq("/x")
		r.Header.Set("Authorization", "Bearer eyJhbGciOiJFUzI1NiJ9.e30.sig")
		rr, _ := serve(mw, r)
		if rr.Code != 401 || obs.last().Source != AuthSourceSPIFFEJWT || obs.last().Result != AuthResultDeny {
			t.Fatalf("code=%d event=%+v", rr.Code, obs.last())
		}
	})

	t.Run("a JWT-SVID bearer is never sent to the keystore as a key", func(t *testing.T) {
		mw, _, seen := mk(IdentityDecision{Presented: true, Source: AuthSourceSPIFFEJWT, Reason: "invalid_token", BearerIsSVID: true})
		r := newReq("/x")
		r.Header.Set("Authorization", "Bearer eyJhbGciOiJFUzI1NiJ9.e30.sig")
		r.Header.Set(header.APIKey, "ak_x")
		rr, _ := serve(mw, r)
		if rr.Code != 200 || len(*seen) != 1 || (*seen)[0] != "ak_x" {
			t.Fatalf("code=%d keystore saw %v", rr.Code, *seen)
		}
	})
}

func TestIdentity_PublicPathsStayPublic(t *testing.T) {
	id := fakeIdentity{decide: func(*http.Request) IdentityDecision {
		t.Fatal("identity must not be consulted for a public path")
		return IdentityDecision{}
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: keyVerifier(nil, new([]string)), Identity: id})
	if rr, _ := serve(mw, newReq("/health")); rr.Code != 200 {
		t.Fatalf("code=%d", rr.Code)
	}
}
