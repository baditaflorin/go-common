package graph

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	enabled       bool
	collectorURL  string
	apiKey        string
	sampleRate    float64
	bufferSize    int
	flushInterval time.Duration
	flushBatch    int
}

// loadConfig reads env vars once. Called from initOnce.
func loadConfig() config {
	c := config{
		enabled:       parseBoolEnv("GRAPH_ENABLED", true),
		collectorURL:  strings.TrimRight(os.Getenv("GRAPH_COLLECTOR_URL"), "/"),
		apiKey:        os.Getenv("GRAPH_API_KEY"),
		sampleRate:    parseFloatEnv("GRAPH_SAMPLE_RATE", 1.0),
		bufferSize:    parseIntEnv("GRAPH_BUFFER_SIZE", 10000),
		flushInterval: time.Duration(parseIntEnv("GRAPH_FLUSH_INTERVAL", 10)) * time.Second,
		flushBatch:    parseIntEnv("GRAPH_FLUSH_BATCH", 500),
	}
	// Note: an empty collectorURL does NOT disable recording. Events
	// still flow into the ring and bump /metrics counters — only the
	// sender goroutine short-circuits the POST. This keeps /metrics
	// useful as a "would I be reporting if a collector were wired?"
	// signal during partial rollout.
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
