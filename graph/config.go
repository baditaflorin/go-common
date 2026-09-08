package graph

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// graphCanonicalCollectorHost is the only non-loopback endpoint allowed to
// receive graph credentials. A scoped key is still secret material.
const graphCanonicalCollectorHost = "fleet-graph.0exec.com"

type config struct {
	enabled       bool
	collectorURL  string
	collectorErr  error
	writerAPIKey  string
	readerAPIKey  string
	targetAliases map[string]string
	sampleRate    float64
	bufferSize    int
	flushInterval time.Duration
	flushBatch    int
}

// loadConfig reads env vars once. Called from initOnce.
func loadConfig() config {
	collectorURL, collectorErr := normalizeCollectorURL(os.Getenv("GRAPH_COLLECTOR_URL"))
	c := config{
		enabled:      parseBoolEnv("GRAPH_ENABLED", false),
		collectorURL: collectorURL,
		collectorErr: collectorErr,
		// GRAPH_API_KEY is deliberately writer-only. In particular, do
		// not fall back to FLEET_API_KEY: a graph event writer should
		// never inherit broad fleet credentials by accident.
		writerAPIKey: strings.TrimSpace(os.Getenv("GRAPH_API_KEY")),
		// Lookup is a separate read capability. It must not reuse the
		// writer credential while graph route auth is being split.
		readerAPIKey:  strings.TrimSpace(os.Getenv("GRAPH_READER_API_KEY")),
		targetAliases: parseTargetAliases(os.Getenv("GRAPH_TARGET_ALIASES")),
		sampleRate:    parseFloatEnv("GRAPH_SAMPLE_RATE", 1.0),
		bufferSize:    parseIntEnv("GRAPH_BUFFER_SIZE", 10000),
		flushInterval: time.Duration(parseIntEnv("GRAPH_FLUSH_INTERVAL", 10)) * time.Second,
		flushBatch:    parseIntEnv("GRAPH_FLUSH_BATCH", 500),
	}
	// Event emission is deliberately all-or-nothing: it is opt-in and
	// requires a valid collector URL plus a dedicated writer key. This
	// prevents a service from silently inheriting observation traffic
	// during a partial credential rollout.
	if c.sampleRate < 0 {
		c.sampleRate = 0
	}
	if c.sampleRate > 1 {
		c.sampleRate = 1
	}
	if c.bufferSize < 64 {
		c.bufferSize = 64
	}
	if c.flushInterval < time.Second {
		c.flushInterval = time.Second
	}
	if c.flushBatch < 1 {
		c.flushBatch = 1
	}
	return c
}

func (c config) eventEmissionEnabled() bool {
	return c.enabled && c.collectorErr == nil && c.collectorURL != "" && c.writerAPIKey != ""
}

// normalizeCollectorURL accepts a root graph endpoint. Remote endpoints
// must be the canonical HTTPS graph host; plain HTTP and alternate HTTPS
// hosts are reserved for explicit loopback endpoints used by local development
// and tests. Query parameters, fragments, and credentials are rejected so the
// endpoint cannot smuggle request state or credentials into graph transport.
func normalizeCollectorURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("graph collector URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("graph collector URL: absolute URL required")
	}
	if u.User != nil {
		return "", fmt.Errorf("graph collector URL: user info is not allowed")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("graph collector URL: query and fragment are not allowed")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("graph collector URL: endpoint must not contain a path")
	}

	u.Scheme = strings.ToLower(u.Scheme)
	switch u.Scheme {
	case "https":
		if !isLoopbackHost(u.Hostname()) {
			if !strings.EqualFold(u.Hostname(), graphCanonicalCollectorHost) {
				return "", fmt.Errorf("graph collector URL: remote collector must be https://%s", graphCanonicalCollectorHost)
			}
			if port := u.Port(); port != "" && port != "443" {
				return "", fmt.Errorf("graph collector URL: remote collector must use the default HTTPS port")
			}
			// Canonicalize case and an explicit :443 so the configured endpoint
			// and the credential's service scope remain one stable authority.
			u.Host = graphCanonicalCollectorHost
		}
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("graph collector URL: HTTPS is required for non-loopback collectors")
		}
	default:
		return "", fmt.Errorf("graph collector URL: unsupported scheme %q", u.Scheme)
	}

	u.Path = ""
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func parseBoolEnv(name string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if v == "" {
		return def
	}
	switch v {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return def
}

func parseFloatEnv(name string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func parseIntEnv(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// parseTargetAliases accepts a comma-separated host=canonical-service-id
// mapping. It is intentionally narrower than a generic URL rewrite:
// aliases apply only to bare go-fleet-* Docker hostnames, never change the
// request destination, and only alter the bounded telemetry target label.
// Invalid entries are ignored so graph observation remains fail-open.
func parseTargetAliases(raw string) map[string]string {
	var aliases map[string]string
	for _, entry := range strings.Split(raw, ",") {
		pair := strings.SplitN(entry, "=", 2)
		if len(pair) != 2 {
			continue
		}
		host := strings.ToLower(strings.TrimSpace(pair[0]))
		target := strings.ToLower(strings.TrimSpace(pair[1]))
		if !validGraphTargetAliasHost(host) || !validGraphServiceID(target) {
			continue
		}
		if aliases == nil {
			aliases = make(map[string]string)
		}
		aliases[host] = target
	}
	return aliases
}

func validGraphTargetAliasHost(host string) bool {
	return strings.HasPrefix(host, "go-fleet-") && validGraphLabel(host)
}

func validGraphServiceID(serviceID string) bool {
	return strings.HasPrefix(serviceID, "fleet-") && validGraphLabel(serviceID)
}

// validGraphLabel accepts the conservative lowercase DNS-label subset used by
// fleet service IDs. Callers lowercase before validation so operator-provided
// aliases remain canonical while punctuation, paths, ports, and domains cannot
// turn this telemetry mapping into an alternate request target.
func validGraphLabel(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}
