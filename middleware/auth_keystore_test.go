package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/header"
)

// stubVerifier lets tests control the keystore response without an
// actual keystore.
type stubVerifier struct {
	verify func(ctx context.Context, key string) (*apikey.VerifyResult, error)
	calls  int
}

func (s *stubVerifier) Verify(ctx context.Context, key string) (*apikey.VerifyResult, error) {
	s.calls++
	return s.verify(ctx, key)
}

func newReq(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	return r
}

func run(t *testing.T, mw Middleware, r *http.Request) (int, string) {
	t.Helper()
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr.Code, rr.Body.String()
}

func TestKeystore_HealthBypass(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("verifier should not be called for /health")
		return nil, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v})
	code, _ := run(t, mw, newReq("/health"))
	if code != http.StatusOK {
		t.Fatalf("/health: want 200 got %d", code)
	}
}

func TestKeystore_GatewayHeaderTrust(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("verifier should not be called when X-Auth-User is set")
		return nil, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v})
	r := newReq("/scan?target=https://x")
	r.Header.Set(header.AuthUser, "operator")
	code, _ := run(t, mw, r)
	if code != http.StatusOK {
		t.Fatalf("gateway-trusted: want 200 got %d", code)
	}
}

func TestKeystore_LocalTokensFastPath(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("verifier should not be called for local tokens")
		return nil, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{
		Verifier:    v,
		LocalTokens: []string{"default_token", "fb_static"},
	})
	r := newReq("/scan?target=x&api_key=default_token")
	code, _ := run(t, mw, r)
	if code != http.StatusOK {
		t.Fatalf("local-token path: want 200 got %d", code)
	}
}

func TestKeystore_KeystoreApproves(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		if k != "ak_valid" {
			return nil, apikey.ErrInvalidKey
		}
		return &apikey.VerifyResult{User: "alice", Scope: "scan"}, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v})
	r := newReq("/scan?target=x&api_key=ak_valid")
	code, _ := run(t, mw, r)
	if code != http.StatusOK || v.calls != 1 {
		t.Fatalf("keystore approve: want 200/1 got %d/%d", code, v.calls)
	}
}

func TestKeystore_KeystoreApproves_SetsAuthTierHeader(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return &apikey.VerifyResult{User: "alice", Scope: "*", Tier: "vetted-pentest"}, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v})
	var seenTier string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenTier = r.Header.Get(header.AuthTier)
		w.WriteHeader(http.StatusOK)
	}))
	r := newReq("/scan?target=x&api_key=ak_valid")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if seenTier != "vetted-pentest" {
		t.Fatalf("downstream handler saw X-Auth-Tier=%q, want %q", seenTier, "vetted-pentest")
	}
}

func TestKeystore_KeystoreApproves_ClobbersClientSuppliedTierHeader(t *testing.T) {
	// A malicious caller sets X-Auth-Tier themselves, hoping the
	// middleware forwards it unchanged. It must be overwritten with the
	// keystore's real answer (empty, here), never left as the client's
	// claimed value — same anti-spoofing guarantee already proven for
	// X-Auth-User/X-Auth-Scope.
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return &apikey.VerifyResult{User: "alice", Scope: "*"}, nil // no Tier granted
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v})
	var seenTier string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenTier = r.Header.Get(header.AuthTier)
		w.WriteHeader(http.StatusOK)
	}))
	r := newReq("/scan?target=x&api_key=ak_valid")
	r.Header.Set(header.AuthTier, "vetted-pentest") // forged by the client
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if seenTier != "" {
		t.Fatalf("client-forged X-Auth-Tier was not clobbered: downstream saw %q", seenTier)
	}
}

func TestKeystore_KeystoreRejects(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return nil, apikey.ErrInvalidKey
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v})
	r := newReq("/scan?target=x&api_key=bogus")
	code, _ := run(t, mw, r)
	if code != http.StatusUnauthorized {
		t.Fatalf("reject: want 401 got %d", code)
	}
}

func TestKeystore_KeystoreUnavailable_FailsClosed(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return nil, errors.New(string(apikey.ErrKeystoreUnavailable.Error()))
	}}
	// Use errors.Is path:
	v.verify = func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return nil, apikey.ErrKeystoreUnavailable
	}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v})
	r := newReq("/scan?target=x&api_key=ak_anything")
	code, _ := run(t, mw, r)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable: want 503 got %d", code)
	}
}

func TestKeystore_MissingTokenIs401(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("verifier should not be called when no token present")
		return nil, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v})
	r := newReq("/scan?target=x")
	code, _ := run(t, mw, r)
	if code != http.StatusUnauthorized {
		t.Fatalf("missing token: want 401 got %d", code)
	}
}

// stubScopeChecker lets tests force a particular VerifyScope outcome.
type stubScopeChecker struct {
	verifyScope func(ctx context.Context, key, claimedScope string) error
	calls       int
}

func (s *stubScopeChecker) VerifyScope(ctx context.Context, key, claimedScope string) error {
	s.calls++
	return s.verifyScope(ctx, key, claimedScope)
}

func TestKeystore_OutOfBandScopeCheck_Match(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("primary verifier must not be hit on gateway-trust path")
		return nil, nil
	}}
	sc := &stubScopeChecker{verifyScope: func(ctx context.Context, key, claimed string) error {
		if key != "ak_real" || claimed != "read" {
			t.Fatalf("unexpected verify-scope args: key=%q scope=%q", key, claimed)
		}
		return nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{
		Verifier:            v,
		OutOfBandScopeCheck: true,
		ScopeChecker:        sc,
	})
	r := newReq("/x?api_key=ak_real")
	r.Header.Set(header.AuthUser, "alice")
	r.Header.Set(header.AuthScope, "read")
	code, _ := run(t, mw, r)
	if code != http.StatusOK {
		t.Fatalf("match: want 200, got %d", code)
	}
	if sc.calls != 1 {
		t.Errorf("expected 1 VerifyScope call, got %d", sc.calls)
	}
}

func TestKeystore_OutOfBandScopeCheck_Mismatch401(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("primary verifier must not be hit on gateway-trust path")
		return nil, nil
	}}
	sc := &stubScopeChecker{verifyScope: func(ctx context.Context, key, claimed string) error {
		return apikey.ErrScopeMismatch
	}}
	mw := TokenAuthKeystore(KeystoreOpts{
		Verifier:            v,
		OutOfBandScopeCheck: true,
		ScopeChecker:        sc,
	})
	r := newReq("/x?api_key=ak_forged")
	r.Header.Set(header.AuthUser, "alice")
	r.Header.Set(header.AuthScope, "admin") // forged
	code, _ := run(t, mw, r)
	if code != http.StatusUnauthorized {
		t.Fatalf("mismatch: want 401 got %d", code)
	}
}

func TestKeystore_OutOfBandScopeCheck_MissingTokenRejects(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("primary verifier must not be hit on gateway-trust path")
		return nil, nil
	}}
	sc := &stubScopeChecker{verifyScope: func(ctx context.Context, key, claimed string) error {
		t.Fatal("VerifyScope must not be called without a token")
		return nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{
		Verifier:            v,
		OutOfBandScopeCheck: true,
		ScopeChecker:        sc,
	})
	r := newReq("/x")
	// Forged gateway headers, but no token in the request — without
	// the key we cannot re-verify, so reject.
	r.Header.Set(header.AuthUser, "alice")
	r.Header.Set(header.AuthScope, "admin")
	code, _ := run(t, mw, r)
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token: want 401 got %d", code)
	}
}

func TestKeystore_TrustPrivateMesh(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("verifier must not be called on the private-mesh trust path")
		return nil, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v, TrustPrivateMesh: true})

	// Private / loopback / ULA peer, no token, no gateway header -> trusted.
	for _, addr := range []string{"127.0.0.1:5555", "172.18.0.4:33333", "10.1.2.3:80", "[fd00::1]:8080"} {
		r := newReq("/scan?target=https://x")
		r.RemoteAddr = addr
		if code, _ := run(t, mw, r); code != http.StatusOK {
			t.Fatalf("private-mesh peer %s: want 200 got %d", addr, code)
		}
	}

	// Public peer with no token must NOT be trusted — the mesh fast path
	// must never leak to the internet.
	r := newReq("/scan?target=https://x")
	r.RemoteAddr = "8.8.8.8:44444"
	if code, _ := run(t, mw, r); code == http.StatusOK {
		t.Fatalf("public peer must not be trusted by TrustPrivateMesh, got 200")
	}
}

func TestKeystore_TrustPrivateMeshOffByDefault(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("verifier must not be reached: missing token denies before lookup")
		return nil, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v}) // TrustPrivateMesh defaults false
	r := newReq("/scan?target=https://x")
	r.RemoteAddr = "127.0.0.1:5555"
	if code, _ := run(t, mw, r); code == http.StatusOK {
		t.Fatalf("default (mesh trust off) must not trust a private peer, got 200")
	}
}

// ─── RequiredTier / TierEnforce ─────────────────────────────────────────

func TestKeystore_RequiredTier_KeystoreVerify_MatchAllows(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return &apikey.VerifyResult{User: "alice", Tier: "vetted-pentest"}, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v, RequiredTier: "vetted-pentest", TierEnforce: true})
	r := newReq("/scan?target=x&api_key=ak_valid")
	if code, _ := run(t, mw, r); code != http.StatusOK {
		t.Fatalf("matching tier, enforced: want 200 got %d", code)
	}
}

func TestKeystore_RequiredTier_KeystoreVerify_Mismatch_Enforced_Denies403(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return &apikey.VerifyResult{User: "alice", Tier: "free"}, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v, RequiredTier: "vetted-pentest", TierEnforce: true})
	r := newReq("/scan?target=x&api_key=ak_valid")
	code, _ := run(t, mw, r)
	if code != http.StatusForbidden {
		t.Fatalf("mismatched tier, enforced: want 403 got %d", code)
	}
}

func TestKeystore_RequiredTier_KeystoreVerify_Mismatch_ShadowMode_StillAllows(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return &apikey.VerifyResult{User: "alice", Tier: "free"}, nil
	}}
	var events []AuthEvent
	obs := observerFunc(func(e AuthEvent) { events = append(events, e) })
	mw := TokenAuthKeystore(KeystoreOpts{
		Verifier: v, RequiredTier: "vetted-pentest", TierEnforce: false, Observer: obs,
	})
	r := newReq("/scan?target=x&api_key=ak_valid")
	code, _ := run(t, mw, r)
	if code != http.StatusOK {
		t.Fatalf("mismatched tier, shadow mode: want 200 (not yet enforced) got %d", code)
	}
	if len(events) != 1 || events[0].Result != AuthResultTierShadowDenied {
		t.Fatalf("expected one AuthResultTierShadowDenied event, got %+v", events)
	}
}

func TestKeystore_RequiredTier_EmptyCallerTier_Denied(t *testing.T) {
	// A key issued before tiering existed (Tier == "") must not satisfy
	// any non-empty RequiredTier — fail closed, not "grandfather them in".
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return &apikey.VerifyResult{User: "alice"}, nil // no Tier
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v, RequiredTier: "vetted-pentest", TierEnforce: true})
	r := newReq("/scan?target=x&api_key=ak_valid")
	if code, _ := run(t, mw, r); code != http.StatusForbidden {
		t.Fatalf("pre-tiering key against a tier gate: want 403 got %d", code)
	}
}

func TestKeystore_RequiredTier_LocalToken_NoEscapeHatch(t *testing.T) {
	// The local-token fast path (default_token, static fallback keys)
	// never verifies a tier. It must never satisfy a tier gate — there is
	// no "the demo key is secretly vetted-pentest" shortcut.
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("verifier should not be called for local tokens")
		return nil, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{
		Verifier: v, LocalTokens: []string{"default_token"},
		RequiredTier: "vetted-pentest", TierEnforce: true,
	})
	r := newReq("/scan?target=x&api_key=default_token")
	if code, _ := run(t, mw, r); code != http.StatusForbidden {
		t.Fatalf("local token against a tier gate: want 403 got %d", code)
	}
}

func TestKeystore_RequiredTier_PrivateMesh_NoEscapeHatch(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("verifier must not be called on the private-mesh trust path")
		return nil, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{
		Verifier: v, TrustPrivateMesh: true,
		RequiredTier: "vetted-pentest", TierEnforce: true,
	})
	r := newReq("/scan?target=x")
	r.RemoteAddr = "127.0.0.1:5555"
	if code, _ := run(t, mw, r); code != http.StatusForbidden {
		t.Fatalf("private-mesh call against a tier gate: want 403 got %d", code)
	}
}

func TestKeystore_RequiredTier_GatewayHeaderTrust_UsesXAuthTierHeader(t *testing.T) {
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		t.Fatal("verifier should not be called when X-Auth-User is set")
		return nil, nil
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v, RequiredTier: "vetted-pentest", TierEnforce: true})

	// nginx forwarded a matching X-Auth-Tier -> allowed.
	r := newReq("/scan?target=x")
	r.Header.Set(header.AuthUser, "operator")
	r.Header.Set(header.AuthTier, "vetted-pentest")
	if code, _ := run(t, mw, r); code != http.StatusOK {
		t.Fatalf("gateway-trusted with matching X-Auth-Tier: want 200 got %d", code)
	}

	// nginx trusted the user but never forwarded X-Auth-Tier (not yet
	// wired, or a non-tiered service's config) -> fails closed, not open.
	r2 := newReq("/scan?target=x")
	r2.Header.Set(header.AuthUser, "operator")
	if code, _ := run(t, mw, r2); code != http.StatusForbidden {
		t.Fatalf("gateway-trusted with absent X-Auth-Tier: want 403 (fail closed) got %d", code)
	}
}

func TestKeystore_RequiredTier_NotSet_NoEffect(t *testing.T) {
	// RequiredTier == "" (the default for every service that doesn't opt
	// in) must behave exactly as before this feature existed.
	v := &stubVerifier{verify: func(ctx context.Context, k string) (*apikey.VerifyResult, error) {
		return &apikey.VerifyResult{User: "alice"}, nil // no Tier at all
	}}
	mw := TokenAuthKeystore(KeystoreOpts{Verifier: v})
	r := newReq("/scan?target=x&api_key=ak_valid")
	if code, _ := run(t, mw, r); code != http.StatusOK {
		t.Fatalf("no RequiredTier configured: want 200 got %d", code)
	}
}

// observerFunc adapts a plain func into an AuthObserver for tests.
type observerFunc func(AuthEvent)

func (f observerFunc) ObserveAuth(e AuthEvent) { f(e) }
