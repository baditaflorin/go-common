package server

import (
	"net/http"

	"github.com/baditaflorin/go-common/fleetfetch"
)

func fetchSourceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, specified := r.URL.Query()["source"]
		if !specified {
			next.ServeHTTP(w, r)
			return
		}
		if len(values) != 1 {
			http.Error(w, "source must be specified once", http.StatusBadRequest)
			return
		}
		source, err := fleetfetch.ParseSource(values[0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx := fleetfetch.WithRequestSource(r.Context(), source)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
