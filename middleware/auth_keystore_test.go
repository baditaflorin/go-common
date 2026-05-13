package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/apikey"
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
	r.Header.Set("X-Auth-User", "operator")
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
