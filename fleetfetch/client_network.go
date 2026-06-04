package fleetfetch

// NetworkEntry is one row of the outbound network log captured for a
// JS-rendered page by go-js-proxy-network and surfaced through the fetch
// cache on the X-FetchCache-Network header. The field set + JSON tags match
// go-common/client.NetworkEntry's fingerprint subset: request-fingerprint
// detectors (analytics / martech / error-monitoring) identify a tool by the
// URLs a page calls (GA /g/collect, Segment api.segment.io, Sentry
// *.ingest.sentry.io) even when the loader is bundled/minified — walk these
// entries' URLs to detect them.
type NetworkEntry struct {
	URL          string `json:"url"`
	Method       string `json:"method"`
	Status       int    `json:"status"`
	ResourceType string `json:"resource_type"`
	Initiator    string `json:"initiator"`
}

// NetworkHeader is the response header the fetch cache emits (js-network
// mode) carrying the JSON-encoded network log. Exported so consumers that
// hold a raw *Response can re-parse it without re-deriving the name.
const NetworkHeader = "X-FetchCache-Network"
