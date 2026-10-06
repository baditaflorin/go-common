# Deployment intent v1

`deploymentintent.V1` is the shared request contract used by deployment
control-plane components. It is a request, not permission. Validation does not
authenticate the caller, query approval or CI systems, verify artifact
provenance, authorize a target pool, reserve capacity, or execute a deployment.

Use `DecodeV1` at an input boundary. It limits the body to 1 MiB, rejects
duplicate and unknown JSON fields, requires exactly one JSON object, and
validates every typed field against the supplied clock. Use `ValidateV1` when
the caller already has a typed value. `DigestV1` produces a stable SHA-256
fingerprint by marshaling the typed contract with UTC timestamps; validate the
intent before using the digest in an authorization decision.

The v1 contract accepts immutable SHA-256 artifact and rollback digests, full
Git object IDs, canonical service IDs, logical pool names, and bounded rolling
rollout settings. It deliberately has no host addresses, credentials, shell
commands, secret paths, or arbitrary runtime configuration. A semantic change
requires a new schema version rather than an ambiguous reinterpretation.
