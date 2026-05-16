// schema.go adds the fleet-wide schema-version signal: a
// monotonically-increasing integer per service that downstream
// consumers can pin against and drift-checkers can compare.
//
// Why this exists: when a service renames a JSON field, removes one,
// or changes its semantic meaning, downstream code keeps reading the
// old key and silently misbehaves. Carrying _schema_version on every
// envelope (response.Envelope) plus exposing it at GET /schema and
// inside GET /capabilities means the catalog scrape can flag drift
// at deploy time rather than at runtime.
//
// Bump policy is documented in response/SCHEMA_VERSIONING.md.
//
// Usage in a service:
//
//	server.Run("go_demo", version, Handler,
//	    server.WithSchemaVersion(3),
//	)
//
// Handlers stamp the version onto every response:
//
//	json.NewEncoder(w).Encode(response.Envelope(payload, server.CurrentSchemaVersion()))

package server

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/baditaflorin/go-common/response"
)

// DefaultSchemaVersion is what services advertise if they don't
// register a version explicitly. 1 is the "I exist but never bumped"
// signal — distinguishable from 0 (which Envelope omits).
const DefaultSchemaVersion = 1

// currentSchemaVersion holds the process-wide schema version. It is
// set by server.New from the Server.SchemaVersion field after options
// have been applied, and read by handlers via CurrentSchemaVersion()
// so they don't have to thread the Server pointer through.
var currentSchemaVersion atomic.Int64

// CurrentSchemaVersion returns the schema version registered for the
// currently-running service (DefaultSchemaVersion when unset). Safe
// to call from any goroutine.
func CurrentSchemaVersion() int {
	v := currentSchemaVersion.Load()
	if v == 0 {
		return DefaultSchemaVersion
	}
	return int(v)
}

// WithSchemaVersion stamps the service's current envelope schema
// version. Bump this monotonically whenever any response shape
// changes in a breaking way (field rename, removal, type change,
// semantic change). Additive new fields do not require a bump.
// See response/SCHEMA_VERSIONING.md.
func WithSchemaVersion(v int) Option {
	return func(s *Server) {
		s.SchemaVersion = v
	}
}

// schemaPayload is what GET /schema returns. Minimal by design — the
// catalog already gets service + version from /capabilities; /schema
// exists as a single-purpose unauthenticated probe for drift checkers
// that don't want to parse the larger /capabilities payload.
type schemaPayload struct {
	Service       string `json:"service"`
	Version       string `json:"version"`
	SchemaVersion int    `json:"schema_version"`
}

// mountSchema wires GET /schema on the server's mux. Called from
// New() after all options have been applied (so SchemaVersion is
// settled) and after currentSchemaVersion has been published.
//
// The endpoint is intentionally unauthenticated — schema_version is
// metadata, not data, and the catalog scrapes services pre-keystore
// during boot.
func mountSchema(s *Server) {
	body, _ := json.Marshal(schemaPayload{
		Service:       s.Config.AppName,
		Version:       s.Config.Version,
		SchemaVersion: s.SchemaVersion,
	})
	s.Mux.HandleFunc("/schema", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}

// publishSchemaVersion writes the Server's schema version (defaulting
// when zero) into both the package-level atomic getter and the
// response package so Envelope can stamp _service without per-handler
// wiring.
func publishSchemaVersion(s *Server) {
	if s.SchemaVersion == 0 {
		s.SchemaVersion = DefaultSchemaVersion
	}
	currentSchemaVersion.Store(int64(s.SchemaVersion))
	response.SetServiceID(s.Config.AppName)
}
