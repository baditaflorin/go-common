package client

// FetchResult is the unified shape returned by Fetch — same struct
// regardless of which backend (html-proxy / js-proxy / direct) served
// the request.
//
// Rich-telemetry fields (Network, ConsoleLogs, Performance) are
// populated only when the caller passes WithNetworkLog(true) (which
// implies a JS render). They are nil on the html-proxy and direct
// paths because those backends do not produce that data. Callers
// should nil-check before walking them; the netextract.go helpers do.
type FetchResult struct {
	URL            string            `json:"url"`            // original request
	FinalURL       string            `json:"final_url"`      // after redirects
	Status         int               `json:"status"`         // final response status
	Headers        map[string]string `json:"headers"`        // final response headers
	Body           string            `json:"body"`           // final response body (rendered DOM when js-proxy)
	RedirectChain  []FetchHop        `json:"redirect_chain"` // all hops INCLUDING final
	CookiesSet     []FetchCookie     `json:"cookies_set"`    // Set-Cookie across all hops
	Backend        string            `json:"backend"`        // "html-proxy"|"js-proxy"|"direct"
	Degraded       bool              `json:"degraded"`       // fell back from requested backend
	DegradedReason string            `json:"degraded_reason,omitempty"`

	// Network is every HTTP request the browser made while rendering
	// the page (document, scripts, XHR, fetch, images, fonts…). Each
	// entry carries URL, method, status, request+response headers,
	// resource_type, initiator and timing. Populated only when
	// WithNetworkLog(true) was passed.
	Network []NetworkEntry `json:"network,omitempty"`

	// ConsoleLogs is the verbatim browser console output (info, warn,
	// error). Most useful: framework dev-mode warnings, stack traces,
	// unhandled-promise rejections. Populated only when
	// WithNetworkLog(true) was passed.
	ConsoleLogs []string `json:"console_logs,omitempty"`

	// Performance carries the CDP lifecycle events and the
	// window.performance.timing snapshot from the rendered page.
	// Populated only when WithNetworkLog(true) was passed.
	Performance *Performance `json:"performance,omitempty"`
}

// HasNetwork reports whether the rich-telemetry fields are present.
// Callers use this as a guard before walking Network/ConsoleLogs/etc.
// — keeps the "did we ask for it?" check at one obvious site instead
// of replicated nil-checks in every consumer.
func (r *FetchResult) HasNetwork() bool {
	return r != nil && r.Network != nil
}
