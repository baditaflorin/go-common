package fleetfetch

import (
	"os"
	"strings"
	"sync"
)

// defaultCaller is the process-wide service identity sent as the
// X-Fleet-Caller header on cache requests that don't set their own via
// WithCaller. The standard go-common server (server.New) sets this from
// the service's AppName, so any service using it propagates its identity to
// the fetch cache — and onward to go-js-proxy / go-html-proxy for
// per-enricher render-load attribution — with no per-service code change.
var (
	defaultCallerMu sync.RWMutex
	defaultCaller   string
)

// SetDefaultCaller sets the process-wide X-Fleet-Caller value. Idempotent
// and safe for concurrent use. server.New calls this automatically from
// AppName; call it directly only when building fleetfetch clients without
// the standard server and unable to set FLEET_SERVICE_ID in the environment.
// An empty or all-invalid id clears the default (header is then omitted
// unless an env fallback or per-client WithCaller applies).
func SetDefaultCaller(id string) {
	defaultCallerMu.Lock()
	defaultCaller = sanitizeCaller(id)
	defaultCallerMu.Unlock()
}

func getDefaultCaller() string {
	defaultCallerMu.RLock()
	defer defaultCallerMu.RUnlock()
	return defaultCaller
}

// callerEnvVars are consulted, in order, when neither WithCaller nor
// SetDefaultCaller supplied an identity — so a service can propagate with no
// Go change at all if its container sets one of these.
var callerEnvVars = []string{"FLEET_SERVICE_ID", "SERVICE_ID"}

// resolveCaller picks the X-Fleet-Caller value for a request in priority
// order: per-client WithCaller → process default (SetDefaultCaller) → env.
// Returns "" when nothing identifies the service, in which case fetch sends
// no header (no behavioural change for un-migrated callers).
func (c *Client) resolveCaller() string {
	if c.caller != "" {
		return c.caller
	}
	if d := getDefaultCaller(); d != "" {
		return d
	}
	for _, k := range callerEnvVars {
		if v := sanitizeCaller(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// sanitizeCaller restricts an identity to a charset safe as both an HTTP
// header value and a Prometheus label (letters, digits, '-' '_' '.' ':'),
// mapping anything else to '_' and capping length. Mirrors go-js-proxy's
// caller sanitiser so a value set here lands as the identical label
// downstream. Returns "" for empty / no-alphanumeric input so resolveCaller
// falls through to the next source rather than emitting a junk label.
func sanitizeCaller(s string) string {
	s = strings.TrimSpace(s)
	const max = 64
	if len(s) > max {
		s = s[:max]
	}
	var b strings.Builder
	b.Grow(len(s))
	hasAlnum := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			hasAlnum = true
		case r == '-' || r == '_' || r == '.' || r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if !hasAlnum {
		return ""
	}
	return b.String()
}
