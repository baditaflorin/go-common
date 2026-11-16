# Provider-neutral request routes

`proxysupplier.NewRouteRouterFromEnv()` enables explicit per-request provider
routing only when `PROXY_ROUTES_FILE` is set. An unset variable leaves existing
`Supplier` defaults unchanged. The router is an additional optional capability;
it does not add retries, choose when residential traffic is acceptable, or
meter bytes.

The strict JSON catalog references existing Webshare pool files and separate
protected files for static proxy URLs. It never accepts inline proxy URLs or
credentials:

```json
{
  "providers": [
    {
      "id": "webshare",
      "type": "webshare_multi",
      "pools_file": "/run/secrets/webshare-pools.json"
    },
    {
      "id": "thunderproxy",
      "type": "static_proxy",
      "pools": [
        {
          "id": "trial",
          "account_id": "trial-account",
          "class": "residential",
          "proxy_url_file": "/run/secrets/thunderproxy-trial-url"
        }
      ]
    }
  ],
  "profiles": [
    {
      "id": "residential-fallback",
      "candidates": [
        { "provider_id": "webshare", "pool_id": "isp" },
        { "provider_id": "thunderproxy", "pool_id": "trial" }
      ]
    }
  ]
}
```

Provider, pool, account and profile IDs must be safe ASCII identifiers. A
catalog is capped at 32 providers, 64 profiles and 32 pools per static provider
or profile chain. Unknown fields, duplicate IDs, duplicate profile candidates,
invalid file references and unknown profile targets are rejected. The
credential-bearing URL stays private to the selected route and is omitted from
selection formatting and safe descriptors.

`ParseRouteIntentQuery` accepts one of `proxy_pool`, `proxy_provider`, or
`proxy_profile`; they are mutually exclusive. `proxy_class` is allowed only
with `proxy_provider`. A pool pin resolves to one exact provider/pool or fails
when the pool ID is missing or ambiguous. Provider plus class resolves only
within that provider and class. A profile resolves to its configured candidate
chain in order. No explicit selector is not a route intent; the caller keeps
using its existing default.

Call `ResolveCandidates` first to inspect safe labels and classes. A residential
candidate must not be selected or used until the service has acquired its
durable byte-budget permit. The caller then passes `allowResidential: true` to
`SelectRoute`; a query parameter alone must never authorize this flag. Every
selection is exact, and `RouteSelection.Report` records typed feedback against
that selection even when providers expose overlapping exit addresses.

For an HTTP client, use `RouteSelection.ProxyURL()` only to configure a guarded
client such as `safehttp.WithProxyURL`. Apply `safehttp.WithConnWrapper` to wrap
the guarded TCP socket with a caller-owned meter; safehttp performs its SSRF,
DNS-rebind and port checks before invoking the wrapper. For browser egress, pass
the selection to `egressproxy.Config.RouteSelection`; use
`WrapUpstreamConn` to meter the raw upstream socket before CONNECT/auth and
`AllowTarget` for the paid profile's destination allowlist. `AllowTarget` runs
only after the package's canonical SSRF guard. Stop the browser context and
close its egressproxy before closing the budget ticket so owned sockets are
closed and outstanding byte accounting is settled before a retry. The wrapper
and allowlist hooks are opt-in; nil preserves existing behavior.

The service owns retries and byte-budget state. Profiles only provide a bounded
ordered set of exact candidates. Static endpoint support does not itself grant
residential access or account for bandwidth.
