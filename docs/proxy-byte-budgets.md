# Proxy socket byte budgets

`proxybudget` is a byte allowance for an explicitly selected proxy route. It is
separate from fleet token billing. It reuses the reservation and wire-counting
design from the search service's existing residential lane without replacing or
opening that lane's ledger.

Open one `Manager` per provider/pool allocation for the life of the process.
Call `Admit(ctx)` before selecting a residential proxy. Pass `ticket.WrapConn`
to the guarded client or egress proxy's post-dial connection wrapper, before
proxy TLS or CONNECT. Every socket for that request, including browser
subresources, must use the same ticket. Close the browser/client connections,
then the ticket; close managers at shutdown. `Close` is idempotent and drains
outstanding socket I/O before final accounting.

The manager enforces positive total, daily, request and in-flight limits.
Admissions reserve and fsync the full request allowance before a socket can
exist. A normal close records actual usage and releases the unused amount.
On a crash, an unfinished allowance is conservatively counted as spent on
restart. This can overcount a failed request, but a crash cannot refill the
budget. Socket counters include proxy and TLS overhead; provider billing may
measure different bytes. Keep headroom in the provider subscription.

State uses an exclusive lock, atomic writes and file/directory sync. A corrupt,
locked or unwritable ledger fails closed. `InitialUsedBytes` seeds only a new
ledger; it never resets existing usage. Creating `DISABLED` in a manager's state
directory stops admissions and further socket I/O without a restart. Removing
it restores admissions subject to the retained limits. Never delete a ledger
to clear a limit. Multiple replicas need distinct allocations whose sum fits
the provider quota; this package does not coordinate a global distributed cap.

`LoadPoolConfigs` accepts a strict, bounded JSON object:

```json
{
  "pools": [{
    "provider_id": "provider-a",
    "pool_id": "residential-a",
    "allowed_hosts": ["example.com"],
    "state_dir": "/var/lib/service/proxy-budgets/provider-a-residential-a",
    "total_budget_bytes": 100000000,
    "daily_cap_bytes": 10000000,
    "max_request_bytes": 1000000,
    "max_inflight": 2
  }]
}
```

Provider/pool identities and state directories must be unique. The file holds
no proxy URLs or credentials. `PoolConfig.AllowsHost` is an exact normalized
match; the consumer must apply it to every target, redirect and browser
subresource, alongside its existing SSRF guards. Loading a budget is not
authorization to use a provider. It neither selects a route nor bypasses an
existing lane token requirement.

`Status` exposes actual bytes, held reservations, limits and active ticket
counts. `Ticket.Usage` reports request bytes. Include safe provider/pool IDs in
the consumer's route receipt and metrics, never credentials or upstream URLs.
