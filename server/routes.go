package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/baditaflorin/go-common/middleware"
)

// routeRecord is a pattern registered through the Server's own helpers.
type routeRecord struct {
	pattern  string
	public   bool // registered with HandlePublic: meant to be served without a key
	internal bool // mounted by the server itself, not by the service
}

// Handle registers handler for pattern on s.Mux, as s.Mux.Handle does, and
// records the pattern so that Start can check it against the public path prefixes
// declared with WithKeystoreAuthPublicPrefixes. The route is meant to be
// protected by the keystore gate: if a declared prefix covers it, requests to it
// skip the gate, and Start returns an error instead of serving it without a key.
//
// Routes registered directly on s.Mux are not recorded and not checked. Use
// HandlePublic for a route that is meant to be served without a key.
func (s *Server) Handle(pattern string, handler http.Handler) {
	s.Mux.Handle(pattern, handler)
	s.record(routeRecord{pattern: pattern})
}

// HandleFunc is Handle for a handler function.
func (s *Server) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	s.Handle(pattern, http.HandlerFunc(handler))
}

// HandlePublic registers handler for pattern on s.Mux, like Handle, for a route
// that is meant to be served without a key: it is what sits under a prefix declared
// with WithKeystoreAuthPublicPrefixes. Start does not count it against the
// declared prefixes. It does not make the route public by itself: declare the
// prefix that covers it.
func (s *Server) HandlePublic(pattern string, handler http.Handler) {
	s.Mux.Handle(pattern, handler)
	s.record(routeRecord{pattern: pattern, public: true})
}

// HandlePublicFunc is HandlePublic for a handler function.
func (s *Server) HandlePublicFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	s.HandlePublic(pattern, http.HandlerFunc(handler))
}

// mount registers a route the server provides itself, and records it like Handle
// does: /readyz, /schema, /metrics/json, /mcp and the cancel route are behind the
// keystore gate too. The routes the gate always exempts (/health, /version,
// /capabilities, /openapi.json, /agent.json) are registered directly.
func (s *Server) mount(pattern string, handler http.Handler) {
	s.Mux.Handle(pattern, handler)
	s.record(routeRecord{pattern: pattern, internal: true})
}

func (s *Server) record(r routeRecord) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	s.routes = append(s.routes, r)
}

// declarePublicPrefixes notes prefixes handed to the keystore gate as public.
func (s *Server) declarePublicPrefixes(prefixes []string) {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	s.publicPrefixes = append(s.publicPrefixes, prefixes...)
}

// validateRoutes reports the recorded routes that the keystore gate protects in
// the service's mind and exempts in fact: those that lie under a public path
// prefix declared with WithKeystoreAuthPublicPrefixes. It returns nil when there
// are none, and when no prefix was declared. It uses the gate's own prefix test,
// middleware.MatchPublicPathPrefix, so "" and "/" are ignored here as there.
//
// The comparison is on the literal part of the pattern, the text before its first
// wildcard: "/c/{token}" is under "/c/", and so is "/c/admin". An exact route
// "/c" is also reported for the prefix "/c/": a router that drops a trailing
// slash serves "/c/" with it. A route that is shorter than the prefix and matches
// more than it ("/", "/{x}") is not reported; it is how a service routes everything
// else, and a request under the prefix reaches the handler registered at the
// prefix first.
func (s *Server) validateRoutes() error {
	s.routesMu.Lock()
	defer s.routesMu.Unlock()
	if len(s.publicPrefixes) == 0 {
		return nil
	}
	var lines []string
	for _, r := range s.routes {
		if r.public {
			continue
		}
		literal := patternLiteralPath(r.pattern)
		if literal == "" {
			continue
		}
		who := fmt.Sprintf("route %q", r.pattern)
		if r.internal {
			who += ", which the server mounts itself,"
		}
		if prefix, ok := middleware.MatchPublicPathPrefix(literal, s.publicPrefixes); ok {
			lines = append(lines, fmt.Sprintf("%s lies under the public path prefix %q", who, prefix))
		} else if prefix, ok := middleware.MatchPublicPathPrefix(literal+"/", s.publicPrefixes); ok {
			lines = append(lines, fmt.Sprintf("%s is the public path prefix %q without its trailing slash, which a router may serve it for", who, prefix))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	sort.Strings(lines)
	return fmt.Errorf("server: refusing to start: %d keystore-gated route(s) would be served without a key, because "+
		"WithKeystoreAuthPublicPrefixes exempts the path prefixes they lie under from the gate:\n  %s\n"+
		"Register a route that is meant to be public with HandlePublic, or declare a prefix that does not cover it",
		len(lines), strings.Join(lines, "\n  "))
}

// patternLiteralPath returns the literal part of a http.ServeMux pattern's path:
// what follows an optional "METHOD " and an optional host, up to the first
// wildcard. "GET example.com/c/{id}" gives "/c/".
func patternLiteralPath(pattern string) string {
	p := strings.TrimSpace(pattern)
	if i := strings.IndexAny(p, " \t"); i >= 0 {
		p = strings.TrimSpace(p[i+1:])
	}
	if i := strings.IndexByte(p, '/'); i > 0 {
		p = p[i:] // a host precedes the path
	} else if i < 0 {
		return ""
	}
	if i := strings.IndexByte(p, '{'); i >= 0 {
		p = p[:i]
	}
	return p
}
