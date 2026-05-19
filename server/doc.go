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
package server
