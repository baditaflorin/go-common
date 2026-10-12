package server

import (
	"net/http"
	"os"

	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/safehttp"
)

func fetchSourceMiddleware(appName string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, specified := r.URL.Query()["source"]
		if !specified {
			configured, err := fleetfetch.ParseSource(os.Getenv(fleetfetch.EnvSource))
			if err == nil && configured == fleetfetch.SourceCommonCrawl &&
				appName != fetchCacheServiceAppName && r.URL.Path != "/selftest" {
				ctx := safehttp.RequireFetchCacheContext(r.Context())
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
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
		// An archive request must not escape to the live origin just because
		// a service's page-fetch client opted out of the ordinary cache path.
		// Only the fetch-cache service itself may bypass this requirement.
		// The hop header is a loop guard, not proof of caller identity, so
		// trusting it on every service would let a caller force an archive
		// request live. /selftest remains a real origin probe by contract.
		if source == fleetfetch.SourceCommonCrawl && appName != fetchCacheServiceAppName && r.URL.Path != "/selftest" {
			ctx = safehttp.RequireFetchCacheContext(ctx)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
