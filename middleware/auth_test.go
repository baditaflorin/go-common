package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestTokenAuth_Header(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer good")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestTokenAuth_RetiredPathDoesNotAuthenticate(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/t/good/something", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for retired path auth, got %d", w.Code)
	}
}

func TestTokenAuth_BlockedSharedCredentialCannotBeConfigured(t *testing.T) {
	blockedFixture := "default" + "_" + "token"
	h := TokenAuth([]string{blockedFixture})(okHandler())
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+blockedFixture)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for retired fallback, got %d", w.Code)
	}
}

func TestTokenAuth_XAPIKeyHeader(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", "good")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestTokenAuth_QueryParam(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/anything?api_key=good", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
}

func TestTokenAuth_QueryParamWithOtherArgs(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/?url=https://example.com&api_key=good", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestTokenAuth_BadToken(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/?api_key=evil", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestTokenAuth_NoToken(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestTokenAuth_FleetProbeBypassesAuth(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	for _, path := range []string{"/health", "/version", "/selftest"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for %s, got %d", path, w.Code)
		}
	}
}

func TestTokenAuth_HeaderTakesPrecedenceOverQuery(t *testing.T) {
	// Header has the bad token, query has the good one. Header wins → 401.
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/?api_key=good", nil)
	req.Header.Set("Authorization", "Bearer evil")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("header should take precedence; expected 401, got %d", w.Code)
	}
}

func TestTokenAuth_IgnoresLegacyPathToken(t *testing.T) {
	// Path tokens are no longer an authentication source. The explicit
	// compatibility query key still authenticates this request.
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/t/evil/route?api_key=good", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy path token should be ignored; expected 200 from query key, got %d", w.Code)
	}
}
