package graph

// SchemaVersion is the wire-format version of Event/Batch payloads.
// Collector accepts current and N+1; bump when adding optional fields.
const SchemaVersion = 1

// Event is one observed fleet HTTP call. Both ends of the call record
// independently — the collector deduplicates by (caller, target, ts).
type Event struct {
	Direction string `json:"dir"`    // "out" or "in"
	Caller    string `json:"caller"` // service slug (or "unknown" / "external:<host>")
	Target    string `json:"target"` // service slug (or "external:<host>")
	Path      string `json:"path"`   // templatised, no query string
	Method    string `json:"method"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Timestamp int64  `json:"ts"` // unix nanos
}

// Batch is the wire payload POSTed to /events.
type Batch struct {
	Service       string  `json:"service"`
	Version       string  `json:"version"`
	SchemaVersion int     `json:"schema_version"`
	Events        []Event `json:"events"`
}

// Service describes a fleet member, returned by Lookup. Mirrors the
// fields in services.json that consumers actually need at runtime.
type Service struct {
	ID         string `json:"id"`
	Mesh       string `json:"mesh"`
	URL        string `json:"url"`
	HealthURL  string `json:"health_url"`
	Kind       string `json:"kind"`
	Healthy    bool   `json:"healthy"`
	LastSeenMs int64  `json:"last_seen_ms"`
}

// Counters are package-level statistics, exposed via Stats() and
// served on /metrics by every fleet service.
type Counters struct {
	EventsRecorded int64 `json:"events_recorded"`
	EventsDropped  int64 `json:"events_dropped"`
	EventsSampled  int64 `json:"events_sampled"`
	BatchesSent    int64 `json:"batches_sent"`
	BatchesFailed  int64 `json:"batches_failed"`
}
