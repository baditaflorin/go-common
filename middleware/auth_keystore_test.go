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
