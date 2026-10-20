package graph

// SchemaVersion is the wire-format version of Event/Batch payloads.
// Collector accepts current and N+1; bump when adding optional fields.
const SchemaVersion = 1

// Event is one emitted fleet HTTP observation. An instrumented caller can
// record an outbound Event and an instrumented target can record an inbound
// Event for the same end-to-end request. The collector aggregates emitted
// events; consumers must not treat their count as a de-duplicated request,
// user, prompt, or LLM-invocation total.
type Event struct {
	Direction string `json:"dir"`    // "out" or "in"
	Caller    string `json:"caller"` // service slug (or "unknown" for an unverified inbound caller / "external:<host>")
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
	// BatchesRejected is the subset of BatchesFailed the collector refused
	// for the batch's own content (403, 400, 413, 415, 422). Those batches are
	// dropped, not retried, so a nonzero value means events were lost.
	BatchesRejected int64 `json:"batches_rejected"`
	// EventsDroppedInvalidIdentity counts events that would have been recorded
	// (Graph emission is configured) but were dropped because GRAPH_SERVICE_ID
	// is set and is not a valid lowercase DNS label, so the identity fails
	// closed. A nonzero, rising value means the service emits nothing until
	// the variable is fixed or unset.
	EventsDroppedInvalidIdentity int64 `json:"events_dropped_invalid_identity"`
	// BatchesSentSPIFFE is the subset of BatchesSent delivered over the SPIFFE
	// mTLS path (GRAPH_SPIFFE_COLLECTOR_URL), without the writer key.
	BatchesSentSPIFFE int64 `json:"batches_sent_spiffe"`
	// BatchesSPIFFEFallback counts batches the SPIFFE path failed to deliver
	// (no answer, or 401, 404, 405, 421) that were then sent with the writer
	// key in the same flush. A rising value means the mTLS path is broken and
	// the key is carrying the traffic.
	BatchesSPIFFEFallback int64 `json:"batches_spiffe_fallback"`
}
