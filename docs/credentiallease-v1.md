# Credential broker client protocol v1

`credentiallease` is the workload-side client contract. The sibling
`credentiallease/broker` package provides the HTTP handler core, while a
separate broker deployment wires it to trust-domain identity, policy, durable
storage, protected audit, and provider adapters. Provider admin credentials
stay with the broker/adapter.

All broker operations use the fleet gateway's `X-API-Key` authentication. The
key must be scoped to the broker hostname and available only to the calling
service. `TaskManager` uses it for task creation and cleanup; its task-bound
lease client reuses it for acquire, revoke, and malformed-response cleanup.
The key is never attached to requests sent to the credential target. The
broker separately verifies the key during task creation and requires the
broker-signed task proof for lease operations.

## Acquire

`POST /v1/leases` over HTTPS. The client sends a short-lived workload proof in
`Authorization: Bearer ...`, a random per-call `Idempotency-Key`, and this JSON
shape:

```json
{
  "task_id": "task identity",
  "audience": "target service identity",
  "resource": "/v1/records/7",
  "target_origin": "https://target.example",
  "actions": ["GET"],
  "ttl_seconds": 300,
  "auth_mode": "api_key_header",
  "auth_header": "X-Api-Key"
}
```

`resource` is one exact canonical escaped HTTP path with no query string,
fragment, wildcard, traversal segment, or encoded path separator. `actions`
contains only allowed HTTP methods such as `GET` or `POST`. `Lease.Do` rejects
every target request whose method or path differs from the approved set. The
broker must match `task_id` to the authenticated workload proof; allowlist the
exact audience, path, target origin, methods, and auth placement; apply its own
TTL maximum; and reject broad or ambiguous requests. The target service must
still enforce body-level object permissions for write operations. For a repeated key from the
same authenticated task, the broker must not create a second lease or reset
the first lease's expiry. The current core rejects a repeated key with
`409 Conflict`, including an identical replay, and rejects a reused key with a
different request body. It never treats the key as authorization. A successful
response is `201 Created`:

```json
{
  "lease_id": "opaque-lease-id",
  "credential": "opaque provider credential",
  "expires_at": "2030-01-01T00:05:00Z"
}
```

The client defaults to a five-minute maximum lease and will not accept a local
maximum above fifteen minutes. The broker must independently enforce an equal
or shorter policy cap. A lease must expire no later than the requested TTL.
Supported delivery modes are `bearer` (no `auth_header`) and
`api_key_header` (one safe, non-authentication, non-cookie HTTP header).

## Use and revoke

The client passes a `Lease` only to a callback, with a context that expires at
the broker-reported deadline. `Lease.Do` sends a request only to the HTTPS
origin fixed at acquisition, adds the configured auth header to a private
request copy, and stops on redirects. When the callback returns, errors, or panics, the
client closes the lease object, clears its in-memory credential buffer on a
best-effort basis, and makes a bounded detached-context request:

```text
DELETE /v1/leases/{lease_id}
Authorization: Bearer <fresh workload proof>
```

The broker should make DELETE idempotent and return `204 No Content` when the
lease is revoked or already expired. The client reports a revoke failure to
the caller; the issuer TTL is the backstop when the broker or provider cannot
confirm immediate revocation. A successful HTTP call is not proof that a
provider supports immediate revoke; each adapter needs its own contract test.

Use `TaskManager.WithTask` for a single application operation. It creates a
broker-owned task ID and proof, runs the callback with a task-scoped client,
then closes the task on success, error, panic, or cancellation. Task closure
rejects new leases and cancels active lease callbacks; each lease cleanup
attempt still requests provider revocation, with provider TTL as the fallback.

## Security requirements for a broker implementation

- Verify issuer, signature, audience, expiry, workload identity, and task
  binding on every acquire and revoke call. Do not trust a caller-supplied task
  ID by itself.
- Enforce resource/action allowlists and provider-specific maximum TTLs. Do not
  permit unrestricted renewals or a universal credential.
- Keep provider credentials and broker administration credentials server-side.
  Return only the single credential bound to the authorized lease.
- Record identity, task, audience, resource, action set, provider, lease ID,
  expiry, and acquire/revoke outcomes in a protected audit stream. The client
  Observer can forward non-secret target-use outcomes (result, status, and
  duration) to the application's protected audit sink. Never record credential
  values, proof tokens, request headers, or response bodies.
- Restrict broker network access and use TLS with trusted roots; use mTLS when
  the runtime identity system supports it. The client rejects cleartext URLs,
  insecure TLS verification, and redirects.
- Bind each lease to one exact HTTPS origin, path, and HTTP method set. The
  client rejects requests to another origin/path/method or with a query string,
  rejects a conflicting `Request.Host`, and it does not follow target redirects.
  It rejects encoded separators and encoded percent bytes to avoid path
  ambiguity through repeated decoding. Target services remain responsible
  for permissions encoded in request bodies.
- Treat a transport timeout during acquire as an uncertain result. Let the
  lease TTL clean up an issuance whose response was lost; do not infer that no
  credential was minted.
- Add provider contract tests for scope, TTL, revoke latency, idempotent
  release, and outage behavior before enabling an adapter.

## Go Common broker core

The `credentiallease/broker` handler implements the v1 wire endpoints and
fails construction unless a verifier, policy, durable store, audit sink, and at
least one issuer are supplied. The verifier must validate proof signature,
issuer, audience, expiry, and signed task binding. The policy must authorize
the exact resource, action set, target origin, auth placement, provider, and
shorter-or-equal TTL. The handler independently checks task binding and rejects
policy scope expansion.

The `Store` contract requires atomic idempotency reservation and durable lease
metadata. Reservations carry an explicit retention deadline of lease expiry
plus a 24-hour retry window. Production implementations must encrypt provider
revocation handles at rest and retain reservations through that deadline. The
provider credential itself is returned once and is not stored by the core. A
duplicate idempotency key receives `409 Conflict`; a caller must request a new
lease only as a distinct authorized operation. A production adapter must
reconcile reserved-but-uncommitted rows after uncertain provider outcomes and
rely on provider expiry when immediate revocation cannot be confirmed.

The package contains no production proof verifier, store, audit backend, or
provider issuer. Its in-memory test fakes are not suitable for deployment. A
separate service wrapper must terminate TLS, restrict network access, provide
the selected identity and provider integrations, and expose no route that
bypasses policy or audit.

Provider credentials are capped at 4 KiB, issuer calls at ten seconds, and
provider revocation calls at five seconds. Storage, identity, and audit
adapters must also honor request-context cancellation. Dependency panics are
converted to a generic server error without writing panic values to logs or
responses; a provider panic after a possible issue still relies on its TTL.

## Integration boundary

Go Fleet should remain the administrative path for existing keystore key
management. This client must not call keystore admin endpoints or turn the
keystore's admin token into an application credential. Fleet Runner only needs
an integration if it later becomes a task workload that consumes broker-issued
credentials; its current key issue/provision/revoke operations stay privileged
and server-side.

Existing applications can adopt the client once a broker endpoint and runtime
workload identity source exist in their trust domain. The shared package now
has fake-provider end-to-end contract coverage, but no production broker
deployment or live provider adapter has been configured; those tests do not
prove live issuance or provider revocation.
