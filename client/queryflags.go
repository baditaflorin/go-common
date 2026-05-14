// queryflags.go translates standard query-string flags into FetchOption
// values. The point: every fleet service that accepts a URL to fetch
// should accept the same flag names for asking for JS rendering, the
// network log, etc. — so callers (catalog UI, hub scrapers, CLI users)
// learn one vocabulary and it works everywhere.
//
// Canonical flag names (use these, do NOT invent service-local
// variants):
//
//	use_js=true        Render the page with JS executed. Returns the
//	                   post-mount DOM via go-js-proxy.
//	use_network=true   Same as use_js plus return the full network log
//	                   (every loaded asset, response headers, sizes,
//	                   timings) plus console + performance, via
//	                   go-js-proxy-network. Implies use_js=true.
//
// Accepted truthy values: "1", "true", "yes", "on" (case-insensitive).
// Anything else is treated as false.
//
// Services wire it in one line at the top of their handler:
//
//	opts := client.OptionsFromQuery(r)
//	res, err := client.Fetch(ctx, target, opts...)
//
// Mode reporting is symmetrical: client.ModeFromQuery(r) returns
// "static" / "rendered_dom" / "rendered_network" so the service can
// echo the mode back in its response shape.

package client

import (
	"net/http"
	"strings"
)

// OptionsFromQuery returns the FetchOption set implied by the
// standard fleet query flags on r. Unrecognised flags are ignored —
// services may parse their own flags from the same URL without
// conflicting with this helper.
//
// Returns an empty slice (NOT nil) if no flags are set, so callers
// can pass the result directly to Fetch without nil-checking.
func OptionsFromQuery(r *http.Request) []FetchOption {
	if r == nil {
		return []FetchOption{}
	}
	q := r.URL.Query()
	opts := []FetchOption{}
	if truthy(q.Get("use_network")) {
		// Implies use_js per fetchproxy.go semantics.
		opts = append(opts, WithNetworkLog(true))
	} else if truthy(q.Get("use_js")) {
		opts = append(opts, WithJSRender(true))
	}
	return opts
}

// ModeFromQuery describes how Fetch will route, given the flags on r.
// Useful for echoing the mode back in the service's response shape so
// the caller can tell at a glance which path produced the data.
//
// Returns one of: "static", "rendered_dom", "rendered_network".
func ModeFromQuery(r *http.Request) string {
	if r == nil {
		return "static"
	}
	q := r.URL.Query()
	switch {
	case truthy(q.Get("use_network")):
		return "rendered_network"
	case truthy(q.Get("use_js")):
		return "rendered_dom"
	default:
		return "static"
	}
}

// FetchCapabilities is the set of standard query flags this package
// understands. Services that use Fetch + OptionsFromQuery should
// surface this on their /capabilities endpoint so the catalog and hub
// can auto-discover which flags work without trying each one.
//
// Keep this in sync with OptionsFromQuery above — adding a flag means
// adding both the parsing AND the capability entry.
var FetchCapabilities = []Capability{
	{
		Name:        "use_js",
		Description: "Render the page with JavaScript executed. Returns post-mount DOM via go-js-proxy.",
		Type:        "bool",
		Values:      []string{"true", "1", "yes", "on"},
		Default:     "false",
		Cost:        "rendered_dom",
	},
	{
		Name:        "use_network",
		Description: "Return the full network log (requests, response headers, sizes, timings) plus console plus performance, via go-js-proxy-network. Implies use_js.",
		Type:        "bool",
		Values:      []string{"true", "1", "yes", "on"},
		Default:     "false",
		Cost:        "rendered_network",
		Implies:     []string{"use_js"},
	},
}

// Capability describes one query-string flag a service accepts.
// Serialised on /capabilities so external tools (catalog, hub) can
// learn the surface area without trying random parameter names.
//
// The shape is intentionally small — adding fields breaks every
// downstream consumer, so keep new metadata in Description for now.
type Capability struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Type        string   `json:"type"`              // "bool" | "string" | "int"
	Values      []string `json:"values,omitempty"`  // allowed values (bool true forms, enum values)
	Default     string   `json:"default,omitempty"` // textual default
	Cost        string   `json:"cost,omitempty"`    // "static"|"rendered_dom"|"rendered_network" or service-specific
	Implies     []string `json:"implies,omitempty"` // other flags this one turns on
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
