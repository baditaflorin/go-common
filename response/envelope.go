// envelope.go adds the fleet-wide schema-version envelope helper.
//
// Why this exists: when a service renames a JSON field, removes one,
// or changes its semantics, downstream consumers break silently —
// they keep reading a key that no longer exists or, worse, reads a
// field whose meaning quietly changed. A monotonically increasing
// integer carried on every envelope lets consumers (catalog, hub,
// audit-graph, sibling services) pin a version, refuse unexpected
// drift, and surface incompatibilities in CI rather than at runtime.
//
// The integer is set per service via server.WithSchemaVersion(N) and
// is exposed at GET /schema and inside GET /capabilities. Handlers
// stamp it onto every response by wrapping the payload:
//
//	response.Envelope(payload, server.CurrentSchemaVersion())
//
// See response/SCHEMA_VERSIONING.md for when to bump and the
// fleet-wide migration recipe.
package response

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// nowFunc is overridable in tests so _emitted_at stays deterministic.
var nowFunc = func() time.Time { return time.Now().UTC() }

// serviceID is set by server.New via SetServiceID so Envelope can
// stamp _service without per-handler wiring. Empty when running
// outside of a server.New process (e.g. unit tests) — in that case
// _service is omitted instead of injected as "".
var serviceID string

// SetServiceID is called by server.New to publish the running
// service's identity to the response package. Idempotent.
func SetServiceID(id string) { serviceID = id }

// Envelope wraps an arbitrary payload in the fleet schema envelope.
//
// The returned map always contains:
//
//   - _schema_version: the integer passed in (omitted when 0)
//   - _service:        the service identity registered by server.New
//                      (omitted when unset or when the payload already
//                      defines _service — see conflict rule below)
//   - _emitted_at:     RFC3339Nano UTC timestamp
//
// plus every top-level field of data:
//
//   - data == nil:              just the envelope keys
//   - data is map[string]any:   keys merged in (envelope wins on its
//                               own meta keys unless data conflicts)
//   - data is a struct/other:   marshalled to JSON, unmarshalled into
//                               a map, then merged. Non-object JSON
//                               (e.g. a bare array or string) is
//                               placed under the "data" key.
//
// Conflict rule: if the user payload already carries one of the
// reserved meta keys (_schema_version, _service, _emitted_at) we log
// a warning to stderr and prefer the user's value. These are
// debug-time problems, not crashes — silently dropping a caller's
// field would be worse than the warning.
func Envelope(data any, schemaVersion int) map[string]any {
	out := map[string]any{
		"_emitted_at": nowFunc().Format(time.RFC3339Nano),
	}
	if schemaVersion != 0 {
		out["_schema_version"] = schemaVersion
	}
	if serviceID != "" {
		out["_service"] = serviceID
	}

	warning := ""
	defer func() {
		emitEnvelope(Event{
			Service:       serviceID,
			SchemaVersion: schemaVersion,
			Warning:       warning,
		})
	}()

	if data == nil {
		return out
	}

	// Fast path: already a map.
	if m, ok := data.(map[string]any); ok {
		warning = mergeInto(out, m)
		return out
	}

	// Otherwise marshal/unmarshal to discover the JSON shape.
	raw, err := json.Marshal(data)
	if err != nil {
		// Marshalling failed — fall back to placing the raw value
		// under "data" via fmt so we still produce a usable envelope
		// rather than crashing the request.
		out["data"] = fmt.Sprintf("%v", data)
		warning = "marshal_failed"
		return out
	}

	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err == nil && asMap != nil {
		warning = mergeInto(out, asMap)
		return out
	}

	// Non-object JSON (array, string, number, bool, null). Park it
	// under "data" so the envelope is still a flat object.
	var asAny any
	if err := json.Unmarshal(raw, &asAny); err == nil {
		out["data"] = asAny
	} else {
		out["data"] = string(raw)
	}
	return out
}

// reservedKeys are the envelope-injected meta keys. mergeInto warns
// (then yields) if user data defines any of these.
var reservedKeys = map[string]struct{}{
	"_schema_version": {},
	"_service":        {},
	"_emitted_at":     {},
}

// mergeInto copies every key from src into dst. On conflict with a
// reserved key, dst's envelope-injected value is overwritten by src
// and a warning is logged — the caller's data is the source of truth
// when both define the same key.
func mergeInto(dst, src map[string]any) (warning string) {
	for k, v := range src {
		if _, reserved := reservedKeys[k]; reserved {
			if _, hasMeta := dst[k]; hasMeta {
				fmt.Fprintf(os.Stderr,
					"response.Envelope: payload key %q conflicts with reserved envelope key; preferring caller value\n",
					k)
				if warning == "" {
					warning = "conflict_" + k
				}
			}
		}
		dst[k] = v
	}
	return warning
}
