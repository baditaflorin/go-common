# Envelope schema versioning

Every fleet service stamps a `_schema_version` integer onto its JSON
responses so downstream consumers (catalog, hub, audit-graph, sibling
services) can pin a version, refuse unexpected drift, and surface
incompatibilities at deploy time rather than at runtime.

The integer is monotonic per service, starts at `1`, and is set at
process boot via `server.WithSchemaVersion(N)`. It is exposed at:

- `GET /schema` — `{"service":"<id>","version":"<ver>","schema_version":N}`
- `GET /capabilities` — same N, alongside the flag list
- `_schema_version` on every payload wrapped with `response.Envelope`

See `go-common/CLAUDE.md` for why this is a library change, not a
per-service patch.

## Wiring (2 lines in a handler)

```go
srv := server.New(cfg, server.WithSchemaVersion(3))
// ... handler body ...
json.NewEncoder(w).Encode(response.Envelope(payload, server.CurrentSchemaVersion()))
```

`server.CurrentSchemaVersion()` reads a package-level atomic set by
`server.New`, so no handler ever has to thread the version through —
the value is correct everywhere the same process runs.

## When to BUMP `schema_version`

Bump the integer by 1 the moment a response shape changes such that a
consumer reading the previous version would break or silently
misinterpret a value:

- Rename a JSON field (`item` → `items`)
- Remove a JSON field that consumers already read
- Change a field's JSON type (`"count": "7"` → `"count": 7`)
- Change a field's semantic meaning (e.g. `latency_ms` started
  measuring something different — wall clock vs. server-side)
- Restructure nesting (flat → nested, or vice versa)
- Change enum values a consumer is likely to switch on (`"ok"` → `"healthy"`)
- Change the units of a numeric field (`ms` → `s`)

If in doubt, bump. The cost of a spurious bump is a stale catalog
entry for one deploy cycle; the cost of a missed bump is a silently
broken downstream.

## When NOT to bump

These are additive and safe:

- Add a new field that didn't exist before
- Add a new endpoint
- Add a new optional query parameter (declare via
  `server.WithCapability`)
- Internal refactors that don't change the wire shape
- Bug fixes that bring the response into alignment with what the
  documented shape always said it would be

## How callers pin a version

Catalog and audit-graph both scrape `/capabilities` on every deploy
and store the `schema_version` alongside the service entry. Two
mechanisms:

1. **Catalog drift check** — `services-registry/services.json` carries
   `schema_version_supported: [N]` per service. The audit job fails
   the deploy if the scraped `schema_version` is not in that list.
   Bumping a service requires updating the registry in the same PR.

2. **Audit-graph drift signal** — every inbound event recorded by
   `graph.Middleware` is already tagged with the producer's
   `schema_version`. The collector can flag any consumer that
   continues to read responses N hours after the producer rotated to
   `N+1` without updating its own catalog entry.

Per-request pinning at the callsite (`X-Accept-Schema-Version: N`) is
intentionally **not** supported in the library — the fleet picks one
schema per service per deploy. Callers that need parallel versions
must wait for the migration cycle below to complete.

## Migration recipe — ship N+1 alongside N for one release cycle

1. **Cut release `N+1` candidate.** Library bump or service code
   change that produces the new shape behind a feature flag or
   alongside the old shape.

2. **Update `service.yaml`** to declare both:

   ```yaml
   schema_version: 4          # the default, what /schema returns
   schema_version_supported:  # what /capabilities advertises
     - 3
     - 4
   ```

   For one release cycle the service answers with the new shape but
   accepts requests against either. Where the response shape varies
   per consumer, dispatch on `X-Schema-Version` header (default to the
   highest).

3. **Wait for consumers to catch up.** Audit-graph will list every
   consumer still reading N. Open PRs against each one. The deploy
   that lands `N+1` does not itself break anything because both
   shapes are live.

4. **Drop N.** Next minor release, remove the old codepath. Update
   `service.yaml`:

   ```yaml
   schema_version: 4
   schema_version_supported:
     - 4
   ```

   Any consumer still reading N will start failing the deploy gate
   from step 2 of *its* registry entry — which is exactly when you
   want to find out.

## Conflict policy inside `response.Envelope`

The envelope reserves `_schema_version`, `_service`, and `_emitted_at`
at the top level of the response. If a payload struct already defines
one of those keys, the caller's value wins and a warning is logged to
stderr. Silently dropping a field would be worse than the warning —
either rename your field or restructure the payload before passing it
to `Envelope`.
