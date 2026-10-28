// Command service is the reference binary of scripts/linkcheck.sh: the smallest fleet
// service, which loads its config, builds the default server stack with server.New and
// serves it. It is never run; the script asks `go list -deps` what it links, with and
// without the opt-out build tags (docs/footprint.md).
package main

import (
	"log"
	"net/http"

	"github.com/baditaflorin/go-common/config"
	"github.com/baditaflorin/go-common/server"
)

func main() {
	srv := server.New(config.Load("linkcheck-service", "0.0.0"))
	srv.Mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
}
