# Deployment authority lease client

`deploymentlease.Client` is the shared mTLS client for the Fleet Deployment
Authority. It uses the maintained `go-common/spiffe` Workload API adapter,
checks the authority's exact SPIFFE server identity, verifies the returned
DSSE authorization against caller-supplied protected Ed25519 keys, and then
acquires a lease bound to the same intent, principal, attempt, policy, and
logical target.

Load signing keys from protected runtime configuration and keep them outside
the public service catalog and task input. Use an HTTPS endpoint reachable on
the private service network. The SPIFFE HTTP transport does not use HTTP proxy
environment variables and does not follow redirects.

Acquisition retries must reuse the same attempt ID. The authority makes those
requests idempotent. Call `Validate` immediately before each deployment
mutation, renew before the lease is near expiry, and call `Finish` with the
verified outcome. If the process exits before `Finish`, the bounded lease
expiry releases the reservation.

The fencing number is monotonic, but this package alone cannot enforce it at a
runtime host. An execution adapter must reject stale fences at the mutation
boundary; a client-side check is not a substitute for target-side fencing.
Until that adapter is deployed and verified, treat this client as a control
plane building block rather than proof that a runtime mutation is fenced.
