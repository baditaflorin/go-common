# Provider-neutral request routes

`proxysupplier.NewRouteRouterFromEnv()` enables explicit per-request provider
routing only when `PROXY_ROUTING_FILE` is set. An unset variable leaves existing
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

## Capture evidence and observability boundaries

A consumer may persist a response and its route receipt with
[capturestore](../capturestore/). The optional fetch-cache writer is a
separate feature: it writes only a fresh, live, complete, non-truncated response
of at most 8 MiB, and storage errors do not turn a successful fetch into a
failed fetch. It verifies an explicit route receipt before setting
`route_verified` and copying the requested selector and actual attempt
provider/account/pool/class/outcome fields. For an ordinary default request,
there is no explicit-route verification claim.

Capture metadata is a bounded provenance record supplied by the consumer.
`route_verified` means that consumer validated the service receipt; it is not
independent network attestation. Capture writers must not include proxy URLs,
credentials, raw request headers or cookies. The fetch-cache writer stores a
SHA-256 fingerprint for forwarded-header identity instead of the headers
themselves. Use the capture-store SDK for its bounded 8 MiB body and 16 KiB
encoded-metadata limits, scoped credential, idempotent writes, and verified
body-download checksum. The optional route API is available from go-common
v0.134.0; the capture-store SDK is in v0.134.4.

The optional fetch-cache writer defines bounded metrics for write outcome
(`fetchcache_capture_store_writes_total`, with fixed stored/error/skipped/disabled
values), duration (`fetchcache_capture_store_write_duration_seconds`), and invalid
writer configuration (`fetchcache_capture_store_config_errors_total`). A writer
must be configured before these metrics are emitted; their absence is not evidence
that capture writes succeeded or that there were no errors.
They have no domain, run, account, pool, provider, or route attempt labels. The shared route package does not itself export attempt,
failure, per-route latency/bytes, or residential budget/spend metrics. Its
catalog-readiness gauges and the [fleet routing dashboard](https://fleet-grafana.0exec.com/d/fleet-proxy-routing/fleet-proxy-routing)
therefore describe pool catalog readiness only; use verified receipts and
bounded consumer telemetry for request evidence. Do not infer route quality or billing from
catalog readiness.

## Verified consumer propagation — 2026-11-17

These smoke checks prove the listed selector paths on the stated replicas;
they are not throughput, CAPTCHA, or supplier-quality benchmarks. A code
release by itself is not deployment evidence.

| Consumer | Version | Deployment scope | Verified selector coverage |
| --- | --- | ---: | --- |
| Go HTML proxy | 0.5.39 | 3 replicas | Both Webshare DC pool pins returned matching live receipts. |
| Go JavaScript proxy | 0.12.8 | 4 replicas | Chrome request-local routing; provider-only selection returned a live receipt. |
| Go network JavaScript proxy | 0.9.5 | 4 replicas | Both Webshare DC pool pins returned matching receipts. |
| Go DuckDuckGo search | 0.4.31 | 2 replicas | Default and both DC pool pins returned results. |
| Go Bing search | 0.4.5 | 2 replicas | Both DC pool pins returned results and matching receipts. |
| Node Duck search | 0.1.7 | 2 replicas | Both DC pins, provider, and profile returned results and matching receipts on each replica. |
| Node Bing search | 0.1.6 | 2 replicas | Both DC pins, provider, and profile returned results and matching receipts on each replica. Its existing default C-backend path still returned 500. |
| Private DomainScope DuckDuckGo sidecar | 0.4.31 | 1 replica | Default and quality-pool pin returned uncached results; conflicting selectors returned 400 without an attempt. |
| DomainScope operator API and frontend | 1.99.362-82d2782 | Stage and production | Staging UI settings roundtrips are recorded below; they verify configuration persistence, not outbound route execution. |

The 19 proxy-service replicas above are a scoped deployment snapshot, not a claim
that all fleet services or all providers support explicit selectors. The two configured
DC pools in this snapshot are both Webshare; dc-failover does not provide an
independent DC supplier. See the
[registry operations guide](https://github.com/baditaflorin/services-registry/blob/main/FLEET.md#shared-proxy-environment-versus-named-webshare-pools)
for the distinction between shared environment proxy settings and per-request
route selection.

DomainScope API and frontend `1.99.362-82d2782` were deployed to staging and
production. In staging, the operator UI set/read/cleared Bing search-engine and
strict `legacy-dc` pool settings for ICANN, CT Names, and OpenINTEL, then restored
the original `auto`/empty selectors with those sources still disabled. BigPicture
was only filled and reset as a draft; it was not submitted. These checks show
settings persistence and restoration, not requests made through the selected
routes. On production, API health and selftest returned 200, and the existing
ICANN and BigPicture batch IDs resumed and progressed while the source controls
remained disabled and Cron357 was preserved. That is batch-recovery evidence,
not a production route-selector smoke test.

Shell probes using short-lived keys were denied with 403 and are not routing
evidence. The staging stale-market selftest returned 503; the targeted UI
roundtrips passed, but that broader staging selftest did not. These checks added
no per-provider, per-account, per-pool, per-class, per-attempt, latency, byte, or
budget metrics. Legacy consumers outside this scoped list remain unported or
unverified; do not infer support from the shared API or from catalog readiness.
