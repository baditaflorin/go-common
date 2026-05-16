// capabilities.go adds the fleet-wide /capabilities endpoint. It tells
// any external caller (catalog UI, hub scraper, CLI user, another
// service) which query-string flags this service understands and
// what they do.
//
// Why this exists: today a user lands on `apikey-scanner.0crawl.com`
// and has to guess that `?use_js=true` is a real knob. They try
// random parameter names until something works (or doesn't). With
// /capabilities the catalog scrapes one URL per service at deploy
// time, caches the flag list in services.json, and the hub renders
// it as a chip row next to each service. Zero hand-curation.
//
// The contract is intentionally minimal — see client.Capability.
// Services register flags at server construction:
//
//	server.Run("go_apikey_scanner", version, Handler,
//	    server.WithCapability(client.FetchCapabilities...),
//	    server.WithCapability(client.Capability{
//	        Name:        "vendor",
//	        Description: "Restrict scan to one vendor (stripe, github, ...)",
//	        Type:        "string",
//	    }),
//	)
//
// Each registration appends; the final list is what /capabilities
// returns. Use client.FetchCapabilities for the standard set
// (use_js, use_network) — declaring them by hand drifts.

package server

import (
	"encoding/json"
	"net/http"

	"github.com/baditaflorin/go-common/client"
)

// WithCapability appends one or more Capability entries to the
// service's advertised flag list. Capabilities are returned verbatim
// on GET /capabilities.
func WithCapability(c ...client.Capability) Option {
	return func(s *Server) {
		s.Capabilities = append(s.Capabilities, c...)
	}
}

// capabilitiesPayload is what /capabilities returns. service + version
// are echoed so a single fleet-wide scrape produces a self-describing
// record per service. schema_version is the same integer GET /schema
// returns — colocating it here means the catalog scrape doesn't need
// a second fetch to learn the envelope contract.
type capabilitiesPayload struct {
	Service       string              `json:"service"`
	Version       string              `json:"version"`
	SchemaVersion int                 `json:"schema_version"`
	Capabilities  []client.Capability `json:"capabilities"`
}

// mountCapabilities wires GET /capabilities on the server's mux. Called
// from New() after all options have been applied.
func mountCapabilities(s *Server) {
	body, _ := json.Marshal(capabilitiesPayload{
		Service:       s.Config.AppName,
		Version:       s.Config.Version,
		SchemaVersion: s.SchemaVersion,
		Capabilities:  append([]client.Capability{}, s.Capabilities...),
	})
	s.Mux.HandleFunc("/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}
