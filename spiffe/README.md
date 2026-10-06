# SPIFFE workload identity

This package uses the maintained `go-spiffe/v2` Workload API clients. It does
not implement SPIFFE token parsing, SVID rotation, trust-bundle validation, or
TLS peer authorization itself.

## Broker mTLS

Use the exact broker SPIFFE ID allowlist. The endpoint is an HTTPS name on the
private service network; do not send the Workload API socket or a task proof
through the public gateway.

```go
ctx, cancel := context.WithTimeout(parent, 10*time.Second)
defer cancel()

broker, err := spiffe.NewHTTPClient(ctx,
    "unix:///run/spire/agent.sock",
    []string{"spiffe://0exec.com/ns/fleet/sa/task-credential-broker"},
)
if err != nil {
    return err
}
defer broker.Close()

tasks, err := credentiallease.NewTaskManager(credentiallease.TaskManagerConfig{
    Endpoint:       "https://task-credential-broker:8443",
    BrokerAudience: "task-credential-broker",
    SPIFFEClient:   broker,
})
if err != nil {
    return err
}
defer tasks.Close()
```

The broker maps the verified client SPIFFE ID to one exact policy principal.
The returned task proof remains required for lease acquire/revoke and is bound
to that principal. Invalid SVIDs, wrong server identities, and unmapped client
IDs fail closed; there is no API-key fallback on this mTLS path.

Keep the `TaskManager` alive while its tasks and task clients are active. Close
tasks before closing the manager so the rotating SVID source remains available
for task cleanup. Do not log SVIDs, task proofs, authenticated headers, or the
Workload API response.

## JWT-SVIDs

`spiffe.Source` fetches short-lived JWT-SVIDs for exactly one requested
audience. Prefer broker-validated JWT-SVIDs only where mTLS is unavailable; use
the X.509 path above for direct service-to-service calls inside the private
mesh. Do not reuse a token across audiences or silently switch to a static key
when the Workload API is unavailable.
