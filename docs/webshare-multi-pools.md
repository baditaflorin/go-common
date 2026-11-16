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
