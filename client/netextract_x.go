package client

import (
	"sort"
	"strings"
)

// XHREndpoint is one runtime-observed API call.
type XHREndpoint struct {
	URL          string `json:"url"`
	Method       string `json:"method"`
	Status       int    `json:"status"`
	ContentType  string `json:"content_type,omitempty"`
	Initiator    string `json:"initiator,omitempty"`
	ResponseSize int64  `json:"response_size"`
}

// XHREndpoints returns one entry per (method, normalised-URL) seen as
// XHR/fetch/WebSocket traffic in the rendered network log. URL
// normalisation collapses numeric and UUID path segments to {id}/{uuid}
// so /users/42, /users/43, /users/44 dedupe to a single
// /users/{id} template — that's the surface a tampering tool wants.
//
// Returns empty slice for nil/empty input.
func XHREndpoints(r *FetchResult) []XHREndpoint {
	if r == nil || len(r.Network) == 0 {
		return nil
	}
	seen := make(map[string]XHREndpoint, len(r.Network))
	for _, e := range r.Network {
		switch strings.ToLower(e.ResourceType) {
		case "xhr", "fetch", "websocket":
		default:
			continue
		}
		tmpl := templatisePath(e.URL)
		key := strings.ToUpper(e.Method) + " " + tmpl
		// Keep the first observation for each (method, template). Later
		// duplicates are usually the same call with different IDs.
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = XHREndpoint{
			URL:          tmpl,
			Method:       strings.ToUpper(e.Method),
			Status:       e.Status,
			ContentType:  headerLookup(e.ResponseHeaders, "Content-Type"),
			Initiator:    e.Initiator,
			ResponseSize: e.ResponseSize,
		}
	}
	out := make([]XHREndpoint, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].URL != out[j].URL {
			return out[i].URL < out[j].URL
		}
		return out[i].Method < out[j].Method
	})
	return out
}
