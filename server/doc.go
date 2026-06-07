// Package server provides the canonical fleet HTTP server bootstrap.
//
// A minimal service main:
//
//	func main() {
//	    cfg := config.Load("go_myservice", "v1.0.0")
//	    srv := server.New(cfg,
//	        server.WithKeystoreAuth("default_token"),
//	        server.WithDependencies(deps),
//	        server.WithMaxBodyBytes(4<<20),
//	    )
//	    srv.Mux.HandleFunc("/api/v1/items", itemsHandler)
//	    log.Fatal(srv.Start())
//	}
//
// server.New wires the full fleet middleware stack (graph, requestID,
// logging, body limit, metrics, promx), /health, /version, /capabilities,
// /schema, /metrics, and /selftest default endpoints automatically.
// Start() performs a graceful SIGTERM drain.
//
// server.New also auto-starts the go-common/obs localhost-only debug
// server (net/http/pprof + a /metrics mirror) bound to 127.0.0.1:6060,
// so every service gets pprof for diagnosing RSS creep / goroutine leaks
// with no extra code. It is loopback-only by design (pprof must never be
// public) and opt-out via DEBUG_ADDR=off or OBS_DISABLE=1.
package server
