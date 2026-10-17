package reqstats_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/go-common/reqstats"
)

func BenchmarkMiddleware(b *testing.B) {
	h := reqstats.Middleware("go_bench", "0.0.0")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}
