# Fleet context — drop-in brief for AI agents

This file is the **canonical** cold-start brief for any AI agent
working inside a baditaflorin fleet service repo. It is maintained in
`services-registry/CLAUDE.md` (the registry is the catalog) and
propagated to every fleet repo via `fleet-runner inject`.

If you find a stale copy that differs from this one, the registry copy
wins — refresh and re-propagate, don't fork it.

Per-service specifics (port, mesh, slug, version, category) live in
the repo's own `service.yaml` + `deploy.yaml` + `README.md`. This file
is intentionally generic — it explains the *fleet*, not any one
service.

## Fleet at a glance

~220 service repos under `github.com/baditaflorin/*`, organised into
three meshes. Each repo declares its mesh via a GitHub topic
(`mesh-0exec` / `mesh-0crawl` / `mesh-pages`) and its category via
`category-<x>`. The canonical catalog is
`services-registry/services.json`; the canonical conventions doc is
`services-registry/FLEET.md` — **read it first** for any fleet-wide
task.

| Mesh         | Domain pattern         | Auth                                       | Used for                              |
|--------------|------------------------|--------------------------------------------|---------------------------------------|
| `mesh-0exec` | `<slug>.0exec.com`     | `?api_key=…` or `X-API-Key` header         | proxy, search, ocr, security          |
| `mesh-0crawl`| `<slug>.0crawl.com`    | path token `/t/<token>/…`                  | domains, recon, web-analysis          |
| `mesh-pages` | static / *.github.io   | none                                       | dashboards, catalogs                  |

Look at `service.yaml` in this repo to see which mesh applies.

## TRL — technology readiness level

Every `services.json` entry may carry a `trl` field 1–9:

| TRL | Band         | Meaning                                                              |
|-----|--------------|----------------------------------------------------------------------|
| 1–3 | toy          | single regex / no tests. Don't depend on it.                         |
| 4–5 | developing   | curated lists, multi-step logic, partial tests.                      |
| 6–7 | real         | RFC-compliant parsing, evidence trails, real test coverage.          |
| 8–9 | production   | battle-tested, cross-checks, SLA-grade.                              |

`trl_ceiling` marks services that **structurally cannot** advance
further (e.g. needs a browser engine, needs paid threat intel).
`trl_assessed_at` older than ~90 days is stale — re-audit.

## Key sibling repos

| Repo                  | Role                                                                           | Visibility |
|-----------------------|--------------------------------------------------------------------------------|------------|
| `services-registry`   | canonical catalog (services.json + FLEET.md + this file)                       | PUBLIC     |
| `go-common`           | shared Go lib — SSRF-safe HTTP, jsbundle recovery, **apikey client**, ua, middleware | PUBLIC |
| `go-apikey-service`   | **the keystore** — issues/verifies/revokes API keys for `mesh-0exec`           | varies     |
| `go-catalog-service`  | renders services.json into `catalog.0exec.com`                                 | PRIVATE    |
| `go_fleet_runner`     | CLI to operate the fleet (`health`, `smoke`, `inject`, `push`, …)              | PRIVATE    |
| `0crawl-platform`     | nginx vhost templates (also embedded in fleet-runner)                          | PRIVATE    |
| `fleet-state`         | live operational state, runbooks, SSH topology                                 | PRIVATE    |

## Auth — how `mesh-0exec` actually authenticates (`go-apikey-service`)

**The keystore is the fleet's single point of compromise.** Treat it
like a CA root: every `0exec` service trusts whatever it says. If
this repo is on `mesh-0crawl` or `mesh-pages`, the keystore does not
apply — skip this section.

Request flow when a caller hits `https://<slug>.0exec.com/...?api_key=<k>`:

1. **nginx vhost** runs an `auth_request` to its `_verify_key` location.
2. **Static fallback** — if `<k>` matches the universal demo key
   hardcoded into the vhost, accept immediately. Survives keystore
   outages for the public demo path.
3. Otherwise nginx POSTs `X-Verify-Key: <k>` to the keystore's `/verify`.
4. Keystore checks SQLite → returns 200 + `X-Auth-User` / `X-Auth-Scope`,
   or 401.
5. On 200, nginx forwards the original request to the service
   container, with `X-Auth-*` headers populated.

**Services do not call the keystore themselves** — nginx already gated
the request. Trust the gateway-injected `X-Auth-*` headers. If you
genuinely need verification inside a service (admin tooling, internal
RPC), use the canonical clients — never handroll HTTP calls:

```go
// Middleware (preferred — gateway header fast-path + keystore fallback + Cache + fail-closed 503):
import "github.com/baditaflorin/go-common/middleware"   // ≥ v0.7.0
// Direct client (only for non-HTTP-handler code):
import "github.com/baditaflorin/go-common/apikey"
c := apikey.New() // reads APIKEY_SERVICE_URL + APIKEY_SERVICE_ADMIN_TOKEN
verifier := apikey.NewCache(c) // 15-min positive cache, no negative cache
result, err := verifier.Verify(ctx, userKey)
```

Keystore outage behaviour (designed-in graceful degradation):
- **Static fallback** in nginx keeps the public demo key working.
- **`apikey.Cache`** in each service keeps recently-verified callers
  working ~15 min.
- **Snapshot data** in `fleet-state/state/snapshot.json` flags the
  keystore as BROKEN once `/health` fails — that's the alert.
- **Recovery procedure**: private `fleet-state/RUNBOOK.md` under
  "keystore outage".

The admin token (`X-Admin-Token` on `/issue`, `/revoke`, `/list`,
`/purge`) is stored as `ADMIN_TOKEN` on the keystore container and
read by clients from `APIKEY_SERVICE_ADMIN_TOKEN`. Rotation playbook:
private `fleet-state/OPS.md`.

## Auth — how `mesh-0crawl` authenticates

`/t/<token>/...` path tokens. Token validation is per-service, not
centralised. Check the repo's handler code — typically a constant
`default_token` plus a list of legitimate tokens loaded from env.

## `go-common` packages — use these, don't reinvent

| Package      | Import path                                       | Purpose                                                 |
|--------------|---------------------------------------------------|---------------------------------------------------------|
| safehttp     | `github.com/baditaflorin/go-common/safehttp`      | SSRF-safe HTTP client, DNS-rebind guard                 |
| ua           | `github.com/baditaflorin/go-common/ua`            | Standard User-Agent builder                             |
| jsbundle     | `github.com/baditaflorin/go-common/jsbundle`      | source-map recovery for scanning JS bundles             |
| apikey       | `github.com/baditaflorin/go-common/apikey`        | keystore client (`Verify`, `Cache`, admin endpoints)    |
| middleware   | `github.com/baditaflorin/go-common/middleware`    | `TokenAuthKeystore` HTTP middleware (≥ v0.7.0)          |

```go
import (
    "github.com/baditaflorin/go-common/safehttp"
    "github.com/baditaflorin/go-common/ua"
)
client := safehttp.NewClient(
    safehttp.WithTimeout(10*time.Second),
    safehttp.WithUserAgent(ua.Build(ServiceID, Version)),
)
// Errors: safehttp.ErrBlocked, safehttp.ErrInvalidScheme, safehttp.ErrMissingHost
u, err := safehttp.NormalizeURL(rawInput)
```

## Service conventions (required for fleet-runner compatibility)

- **Port**: from `PORT` env; fallback to a build-time constant; must
  match `service.yaml`, compose, and `deploy.yaml`.
- **Health**: `GET /health` → `{"status":"ok","service":"<id>","version":"<ver>"}`.
- **Version**: `GET /version` → `{"version":"<ver>"}`.
- **Metrics**: `GET /metrics` (Prometheus).
- **Gateway health**: `GET /_gw_health` is added by the nginx
  template, not by the service — don't re-implement.
- **User-Agent**: `ua.Build(ServiceID, Version)`.
- **Docker image**: `ghcr.io/baditaflorin/<id>:<version>` (no `v`
  prefix on the tag).
- **Tagging**: `git tag <version>` (no `v` prefix), e.g. `1.2.3`.
- **service.yaml** must keep: `id`, `name`, `version`, `port`,
  `category`, `health` block, `test` block.

## fleet-runner

Binary at `/usr/local/bin/fleet-runner` on **Builder LXC 108**. From
any workspace dir on that LXC:

```
fleet-runner health [--insecure]             # /health on all live services
fleet-runner smoke  [--insecure]             # GET example_url on all services
fleet-runner build-test                      # go test ./... in every workspace
fleet-runner update-dep <mod@ver>            # bump dep across all repos
fleet-runner inject <src> <dest>             # copy a file into every repo
fleet-runner exec   "<cmd>"                  # shell command in every repo
fleet-runner push   "<msg>"                  # commit+push all dirty repos
fleet-runner nginx-render                    # regenerate vhosts from templates
fleet-runner new-service <name> <port> [cat] # scaffold new service
fleet-runner stats                           # audit log + token usage summary
```

All commands accept `--tokens-used N --model NAME` for LLM accounting.

## Infrastructure topology

| Target          | SSH                                                            |
|-----------------|----------------------------------------------------------------|
| Bastion         | `ssh root@0docker.com`                                         |
| Builder LXC 108 | `ssh root@0docker.com 'pct exec 108 -- bash -lc "<cmd>"'`      |
| Dockerhost VM   | `ssh -J root@0docker.com ubuntu_vm@10.10.10.20`                |
| Webgateway      | `ssh -J root@0docker.com florin@10.10.10.10`                   |

- **Builder LXC 108** is a Proxmox container on `0docker.com`. Hosts
  per-service build workspaces at `/root/workspace/go_*/` and the
  `fleet-runner` binary.
- **Dockerhost VM** runs the service containers. Compose dirs:
  `/opt/services/<repo>/`, `/opt/security/<repo>/`,
  `/home/ubuntu_vm/pentest/<repo>/`.
- **Webgateway** runs nginx (the public TLS terminator) and the
  keystore-aware `auth_request` flow.
- Build + push: `docker buildx build --platform linux/amd64 --provenance=false -t ghcr.io/baditaflorin/<id>:<ver> --push .`

Operational topology and credentials are in **private**
`fleet-state/OPS.md` — never commit SSH targets, IPs, or tokens to
service repos.

## Fleet-wide changes — change `go-common`, not consumers

The cardinal rule when you'd otherwise touch every service: **modify
the library and bump the dep.** A `go-common` patch plus
`fleet-runner update-dep github.com/baditaflorin/go-common@vX.Y.Z`
beats 130 PRs.

## Local workflow

- Local workspace root: `/Users/live/Documents/Codex/2026-05-08/`.
  Sibling repos sit next to this one — read them directly when you
  need to understand a dependency.
- CI: there is none. Husky pre-commit hooks + local `npm run smoke`
  (Node repos) or `go test ./...` (Go repos) are the gate. Don't
  scaffold GitHub Actions build workflows.
- Supply chain: prefer npm packages ≥ 3 days old over `@latest` —
  accept known CVEs over zero-day supply-chain injection.
