package server_test

import (
	"net/http"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/server"
)

// New wires the whole fleet stack (request id, logging, body limit, metrics,
// /health, /version, /selftest, ...). Start blocks until SIGTERM and then
// drains, so this example is compiled but not run.
func ExampleNew() {
	cfg := config.Load("go_example", "v0.1.0")
	srv := server.New(cfg)
	srv.Mux.HandleFunc("/api/v1/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})
	// log.Fatal(srv.Start())
}
