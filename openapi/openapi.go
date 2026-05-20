// Package openapi provides types and helpers for building OpenAPI 3.0.3
// specification documents that fleet services expose at /openapi.json.
//
// Typical usage:
//
//	spec := openapi.New(cfg.AppName, cfg.Version)
//	// optionally enrich with handler annotations:
//	openapi.ScanDir(".", spec)
//	srv := server.New(cfg, server.WithOpenAPI(spec))
package openapi

import "encoding/json"

// Spec is the root OpenAPI 3.0.3 document.  Only the fields fleet services
// use are included — the type is intentionally minimal.
type Spec struct {
	OpenAPI    string              `json:"openapi"`              // always "3.0.3"
	Info       Info                `json:"info"`
	Paths      map[string]PathItem `json:"paths"`
	Components *Components         `json:"components,omitempty"`
}

// Info carries the service identity block.
type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// PathItem groups the operations available on a single URL path.
type PathItem struct {
	Get    *Operation `json:"get,omitempty"`
	Post   *Operation `json:"post,omitempty"`
	Put    *Operation `json:"put,omitempty"`
	Delete *Operation `json:"delete,omitempty"`
	Patch  *Operation `json:"patch,omitempty"`
}

// Operation is a single HTTP method on a path.
type Operation struct {
	Summary     string              `json:"summary,omitempty"`
	Description string              `json:"description,omitempty"`
	Tags        []string            `json:"tags,omitempty"`
	Parameters  []Parameter         `json:"parameters,omitempty"`
	Responses   map[string]Response `json:"responses"`
}

// Parameter describes a path, query, header, or cookie parameter.
type Parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"` // "path", "query", "header", "cookie"
	Description string  `json:"description,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// Response describes a single HTTP response.
type Response struct {
	Description string                     `json:"description"`
	Content     map[string]MediaTypeObject `json:"content,omitempty"`
}

// MediaTypeObject wraps a Schema for a given media-type key.
type MediaTypeObject struct {
	Schema *Schema `json:"schema,omitempty"`
}

// Schema is a simplified JSON Schema subset used inside OpenAPI.
type Schema struct {
	Type       string             `json:"type,omitempty"`
	Properties map[string]*Schema `json:"properties,omitempty"`
	Example    interface{}        `json:"example,omitempty"`
}

// Components holds reusable schema definitions.
type Components struct {
	Schemas map[string]*Schema `json:"schemas,omitempty"`
}

// New creates a Spec pre-populated with the four standard fleet endpoints
// that every container service exposes via go-common/server:
//
//   - GET /health      → 200 {"status":"healthy"}
//   - GET /version     → 200 text/plain
//   - GET /selftest    → 200 {}
//   - GET /openapi.json → 200 (this document, self-referential)
func New(title, version string) *Spec {
	s := &Spec{
		OpenAPI: "3.0.3",
		Info: Info{
			Title:   title,
			Version: version,
		},
		Paths: make(map[string]PathItem),
	}

	// /health
	s.Paths["/health"] = PathItem{
		Get: &Operation{
			Summary: "Health check",
			Tags:    []string{"fleet"},
			Responses: map[string]Response{
				"200": {
					Description: "Service is healthy (or degraded — HTTP 200 either way)",
					Content: map[string]MediaTypeObject{
						"application/json": {
							Schema: &Schema{
								Type: "object",
								Properties: map[string]*Schema{
									"status":  {Type: "string", Example: "healthy"},
									"service": {Type: "string"},
									"version": {Type: "string"},
								},
							},
						},
					},
				},
			},
		},
	}

	// /version
	s.Paths["/version"] = PathItem{
		Get: &Operation{
			Summary: "Service version",
			Tags:    []string{"fleet"},
			Responses: map[string]Response{
				"200": {
					Description: "Semver string of the running binary",
					Content: map[string]MediaTypeObject{
						"text/plain": {},
					},
				},
			},
		},
	}

	// /selftest
	s.Paths["/selftest"] = PathItem{
		Get: &Operation{
			Summary: "Self-test probe",
			Tags:    []string{"fleet"},
			Responses: map[string]Response{
				"200": {
					Description: "Self-test suite passed (or default 200 when no suite is registered)",
					Content: map[string]MediaTypeObject{
						"application/json": {
							Schema: &Schema{Type: "object"},
						},
					},
				},
			},
		},
	}

	// /openapi.json (self-referential)
	s.Paths["/openapi.json"] = PathItem{
		Get: &Operation{
			Summary: "OpenAPI spec",
			Tags:    []string{"fleet"},
			Responses: map[string]Response{
				"200": {
					Description: "OpenAPI 3.0.3 document for this service",
					Content: map[string]MediaTypeObject{
						"application/json": {},
					},
				},
			},
		},
	}

	return s
}

// AddRoute adds or replaces an operation for method+path in the spec.
// method is case-insensitive ("GET", "get", etc.).
func (s *Spec) AddRoute(method, path string, op Operation) {
	item := s.Paths[path] // zero-value PathItem if missing

	switch toUpper(method) {
	case "GET":
		item.Get = &op
	case "POST":
		item.Post = &op
	case "PUT":
		item.Put = &op
	case "DELETE":
		item.Delete = &op
	case "PATCH":
		item.Patch = &op
	}

	s.Paths[path] = item
}

// JSON serialises the spec to indented JSON bytes.
func (s *Spec) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// toUpper is a zero-dependency ASCII upper-case for HTTP method strings.
func toUpper(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		b[i] = c
	}
	return string(b)
}
