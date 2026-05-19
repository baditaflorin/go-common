package server_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/server"
)

// TestWithIdempotencyKey_Option verifies the server.Option threads
// the idempotency middleware into the chain so a registered handler
// is deduped on retry.
func TestWithIdempotencyKey_Option(t *testing.T) {
	var count int32
	srv := server.New(&config.Config{AppName: "test", Version: "0.0.0", Port: "0"},
		server.WithIdempotencyKey(middleware.IdempotencyConfig{}),
	)
	srv.Mux.HandleFunc("/do", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	// Mimic Server.Start's chain construction so we exercise the
	// option's effect end-to-end.
	h := middleware.Chain(srv.Mux, srv.Middlewares...)

	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/do", nil)
		r.Header.Set(middleware.IdempotencyKeyHeader, "k")
		return r
	}

	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, req())
	if w1.Code != http.StatusCreated {
		t.Fatalf("first call: status %d", w1.Code)
	}
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req())
	if w2.Code != http.StatusCreated {
		t.Fatalf("replay call: status %d", w2.Code)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("handler should be deduped via the server option, ran %d times", got)
	}
	if w2.Header().Get(middleware.IdempotencyCachedHeader) != "true" {
		t.Fatalf("replay missing Idempotency-Cached header")
	}
}
