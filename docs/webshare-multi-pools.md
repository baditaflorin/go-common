# Named Webshare proxy pools

`PROXY_SUPPLIER=webshare_multi` configures independent Webshare account-plan
catalogs. Set `WEBSHARE_POOLS_FILE` to a JSON file readable by the service. API
keys must be stored in separate key files and referenced by absolute path; the
JSON schema rejects inline or unknown fields.

```json
{
  "pools": [
    {
      "id": "dc-legacy",
      "account_id": "legacy-account",
      "class": "dc",
      "plan_id": "123456",
      "api_key_file": "/run/secrets/webshare-legacy.key"
    },
    {
      "id": "dc-quality",
      "account_id": "quality-account",
      "class": "dc",
      "plan_id": "789012",
      "api_key_file": "/run/secrets/webshare-quality.key"
    },
    {
      "id": "isp",
      "account_id": "quality-account",
      "class": "residential",
      "plan_id": "345678",
      "api_key_file": "/run/secrets/webshare-isp.key"
    }
  ]
}
```

The IDs above are examples only. `plan_id` is a positive decimal integer encoded
as a JSON string. Each account-plan pair must be unique. A pool may use a
positive `weight` to override its default share. With no override, the default
weight is the number of eligible catalog entries; this is a balancing weight,
not a billing or provider-quota measurement. At most 32 pools are accepted.

The compatibility `Supplier.ProxyURL()` path selects only `class: "dc"` pools.
It combines eligible DC pools using smooth weighted round robin and fails
closed when no DC address is available. Consumers that own policy and retry
budgets can type-assert `WebsharePoolSelector`, select a class or named pool,
and report the result against the exact `WebshareSelection`. Residential
selection is always explicit and should be made only by the consumer's own
typed fallback policy. The shared supplier does not retry requests or decide
when a consumer may use a more expensive pool.

`WEBSHARE_DEFAULT_POOL_ID` is an optional process-level route pin for services
that need a named DC profile. It must match a configured `class: "dc"` pool;
an unknown ID or a residential ID makes the supplier unavailable and fails
closed. When unset, the existing capacity-weighted DC selection is unchanged.
This setting does not enable browser-level residential egress or request-level
pool switching. `proxysupplier_webshare_pool_default` identifies a pinned
default pool without exposing plan IDs, key paths, or proxy credentials.

`HTTPClientForSelection` pins an HTTP client to the exact selected proxy entry.
Build a client for each selection/attempt; do not reuse it for another selection
from the same pool. It accepts a custom dial function and redirect policy so
consumers can preserve a request byte meter and destination allowlist. Close
idle connections when discarding it. Catalog pagination is constrained to the
same HTTPS origin and path, same `plan_id`, and bounded page, entry, and
response sizes.

`WebsharePools` and `WebsharePoolsSnapshot` expose only bounded pool labels and
health. Keys, key paths, plan IDs, and proxy URLs are excluded. `Close` stops
all catalog refresh goroutines created by the multi-pool supplier.

## Outcome kinds, ASN exclusion, pacing and capacity

Report results with an outcome kind when the caller can tell them apart:
`MarkSelectionOutcome(selection, proxysupplier.OutcomeChallenge)` for a bot
challenge, 403 or 429; `OutcomeTransport` for a dial, TLS, timeout or proxy
error; `OutcomeUpstream` for a 5xx or a body that cannot be parsed; `OutcomeOK`
for a usable answer. Every non-OK outcome cools the exit IP down; only
`OutcomeChallenge` counts against its ASN and /24, so a target outage cannot
quarantine a network. The bool `MarkSelectionResult` keeps counting false as a
network failure.

A retry that passes the ASN that just challenged it to
`SelectPoolExcludingASNs` gets another ASN whenever any other ASN has an
eligible IP in any pool of the class; the excluded ASN is only the last resort.

Per-ASN pacing is off unless configured. With
`WEBSHARE_PACING_PER_IP_RATE=0.0167` (one request per IP per minute; or
`Config.WebsharePacing`, or `proxysupplier.ConfigureWebsharePacing`), each ASN
may send `PerIPRate` x its IP count requests per second across every pool
(floored at `WEBSHARE_PACING_MIN_ASN_RATE`, bursts of `WEBSHARE_PACING_BURST`,
default 2). When every eligible ASN is out of tokens a selection returns a
`*PacedError` with `RetryAfter`; `proxysupplier.SelectWait(ctx, fn)` sleeps and
retries until ctx is done. Quarantined ASNs never slow the others down: pacing
is per ASN, and the healthy ASNs keep their own buckets.

`HealthyCapacity(poolID)` and `HealthyCapacityAll()` (`WebshareCapacityReporter`)
say how many non-quarantined ASNs still have an eligible IP, how many IPs that
is, and how soon a quarantined ASN probes again.
