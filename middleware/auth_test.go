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

func TestTokenAuth_LegacyPath(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/t/good/something", nil)
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

func TestTokenAuth_HealthBypassesAuth(t *testing.T) {
	h := TokenAuth([]string{"good"})(okHandler())
	for _, path := range []string{"/health", "/version"} {
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

func TestTokenAuth_PathTakesPrecedenceOverQuery(t *testing.T) {
	// Legacy path has bad token, query has good. Path wins → 401.
	h := TokenAuth([]string{"good"})(okHandler())
	req := httptest.NewRequest("GET", "/t/evil/route?api_key=good", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("legacy path should take precedence over query; expected 401, got %d", w.Code)
	}
}
