package graph

import (
	"net/http"
	"time"
)

// RoundTripper wraps next with outbound observation. Every request
// that completes (or fails) emits one outbound Event. The wrapper
// preserves the underlying transport's behaviour exactly; it only
// observes timing and metadata.
//
// safehttp.NewClient wraps its transport with this so every fleet
// outbound call is automatically recorded.
func RoundTripper(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &observingTransport{next: next}
}

type observingTransport struct {
	next http.RoundTripper
}

func (t *observingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Avoid recursion: if the request is itself going to the collector
	// we must not record it (would loop on every batch flush).
	if isCollectorURL(req) {
		return t.next.RoundTrip(req)
	}
	if !Enabled() {
		return t.next.RoundTrip(req)
	}
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	latency := time.Since(start).Milliseconds()
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	target := "external:unknown"
	if req.URL != nil {
		target = targetFromHost(req.URL.Host)
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	path := "/"
	if req.URL != nil {
		path = templatisePath(req.URL.Path)
	}
	Record(Event{
		Direction: "out",
		// Caller filled in by Record from package identity.
		Target:    target,
		Path:      path,
		Method:    method,
		Status:    status,
		LatencyMs: latency,
	})
	_ = err // surface the original error/response; observation is fire-and-forget
	return resp, err
}

// isCollectorURL returns true if req is going to the configured
// graph collector. Used to break the recursive loop where flushing
// events would itself be observed and produce more events.
func isCollectorURL(req *http.Request) bool {
	s := ensureInit()
	if s == nil || s.cfg.collectorURL == "" || req.URL == nil {
		return false
	}
	// Cheap substring check: collector URL contains scheme+host,
	// request URL.String() does too. We compare hostnames.
	colHost := hostFromURL(s.cfg.collectorURL)
	return colHost != "" && colHost == req.URL.Host
}

func hostFromURL(rawURL string) string {
	// minimal scheme:// stripper, avoids importing net/url to keep
	// this hot-path lean.
	for _, sep := range []string{"://"} {
		if i := indexOf(rawURL, sep); i >= 0 {
			rawURL = rawURL[i+len(sep):]
			break
		}
	}
	for i := 0; i < len(rawURL); i++ {
		if rawURL[i] == '/' {
			return rawURL[:i]
		}
	}
	return rawURL
}

func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}
