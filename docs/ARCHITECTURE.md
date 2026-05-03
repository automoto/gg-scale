# ggscale — Phase 0 Architecture

This document describes what's actually running in the dev stack today
(Phase 0). Phase 1+ scope lives in [`mvp.md`](mvp.md) and [`ROADMAP.md`](ROADMAP.md);
this file is rewritten as those phases land.

## Deployment topology

There are two distinct compose profiles:

- **lite** (default `make up`) — control plane + observability + auxiliary
  services. No Kubernetes. Sufficient for HTTP API work.
- **k8s** (`make up-k8s`) — adds k3s (single-node, `--network=host`) and an
  Agones install job. Required for the Agones e2e smoke test.

```
                   ┌────────────────────────────────────────┐
                   │ ggscale-server :8080  (Go, chi router) │
                  │  /v1/healthz · /v1/* · /metrics        │
                  │  /v1/dashboard (HTMX + templ)          │
                   │  + cache.Store (memory or olric)       │
                   └─────────────────────┬──────────────────┘
                                         │
                                   postgres:5432
                                   (init via
                                    migrate init
                                    container)
```

## Lite-profile services

| Service | Image | Port | Role |
|---|---|---|---|
| `ggscale-server` | local `ggscale-server:dev` | 8080 | Control-plane HTTP API (chi router, slog JSON, prometheus `/metrics`). |
| `postgres` | `postgres:17` | 5432 | Primary DB. `pg_isready` healthcheck. |
| `migrate` | `migrate/migrate:v4.19.1` | — | One-shot init container; applies `db/migrations/*.up.sql` against postgres before `ggscale-server` starts. |
| `prometheus` | `prom/prometheus:v3.1.0` | 9090 | Scrapes `ggscale-server:8080/metrics`. Browse counters at `http://localhost:9090/graph`. |
| `mailhog` | `mailhog/mailhog:v1.0.1` | 1025 / 8025 | SMTP sink + web UI for capturing signup/verify mail (Phase 1+). |
| `stripe-mock` | `stripe/stripe-mock:v0.197.0` | 12111 / 12112 | Local Stripe API mock for the billing flows that land in Phase 3. Pre-wired now to avoid later compose churn. |
| `ggscale-server` dashboard | local `ggscale-server:dev` | 3001 -> 8080 | HTMX + templ dashboard at `/v1/dashboard/login` for tenant/project/API-key bootstrap and API-key lifecycle management. |

## Dashboard bootstrap

On first run (no dashboard users in the DB), `ggscale-server` generates a
one-time setup token. Both compose files write this token to a file via
`DASHBOARD_BOOTSTRAP_TOKEN_FILE=/run/ggscale/bootstrap.token`, which is
bind-mounted to `./data/` on the host:

```shell
cat ./data/bootstrap.token
```

Navigate to `http://localhost:3001/v1/dashboard/setup`, paste the token into
the **Bootstrap token** field, and create the first platform-admin account.
The token is kept out of structured logs and out of URLs — log aggregators
(Loki, Datadog, CloudWatch) and browser history would otherwise retain the
plaintext value. If `DASHBOARD_BOOTSTRAP_TOKEN_FILE` is unset the token falls
back to stderr only.

## K8s-profile services

| Service | Image | Role |
|---|---|---|
| `k3s` | `rancher/k3s:v1.30.6-k3s1` | Single-node control plane + agent. Runs `--network=host`, `--privileged`, with Traefik and ServiceLB disabled. Writes the kubeconfig to `./.k3s/kubeconfig.yaml`. |
| `agones-install` | `bitnami/kubectl:1.30` | One-shot job that applies `k8s/agones-install.yaml` (pinned Agones v1.42.0) and waits for the controller + allocator to roll out. |

The Agones install manifest is committed verbatim under `k8s/agones-install.yaml`
(top-of-file comment records the source URL and version). Bumping Agones is a
re-fetch and a PR.

## Why `--network=host` for k3s

The MVP requires that game-server UDP `hostPort`s assigned by Agones be
reachable directly from the host OS — no Docker bridge, no port-forward proxy.
That's the `--network=host` contract. It's the substrate the Phase 0 e2e smoke
test exercises (`TestAgonesAllocation_AssignsHostPort_ReachableViaUDP`):

1. Apply a `GameServer` CRD via the Agones Go client.
2. Wait for `Status.State == Ready`.
3. Read `Status.Ports[0].Port` (a dynamic UDP port on the host).
4. Open `net.Dial("udp", "127.0.0.1:<port>")` and send a packet.
5. Assert the simple-game-server image echoes it back.

If `--network=host` were replaced with bridged networking + port-forwards
(e.g. k3d), step 5 would silently fail in production with real game traffic.
That's the parity contract the MVP §"CI parity" forbids breaking.

### macOS caveat

Docker Desktop's host networking on darwin is unreliable (the daemon runs in
a Linux VM). For `make up-k8s` on macOS, run **Colima** with host networking
exposed:

```
brew install colima
colima start --network-address --cpus 4 --memory 8
```

Linux CI (and Linux dev workstations) just work without Colima.

## Configuration

The server reads a fixed list of environment variables enumerated in
`internal/config/config.go`. Every variable is also documented in
`.env.example`; the test `TestEnvExample_HasNoDrift` fails the build if the
two diverge.

| Var | Required | Default | Purpose |
|---|---|---|---|
| `DATABASE_URL` | yes | — | Postgres connection string. |
| `HTTP_ADDR` | no | `:8080` | HTTP listen address. |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `ENV` | no | `dev` | `dev`, `staging`, `prod`. |
| `DASHBOARD_DISABLED` | no | `false` | Disable the server-rendered dashboard when `true`. |
| `DASHBOARD_BOOTSTRAP_TOKEN_FILE` | no | (empty) | Optional path for the first-run bootstrap token; written mode 0600. |
| `DASHBOARD_COOKIE_SECURE` | no | `false` | Sets the `Secure` flag on dashboard session cookies. |
| `CACHE_BACKEND` | no | `memory` | Cache.Store backend. `memory` (default; in-process map; zero networking deps; covers dev, single-VM self-host, tests) or `olric` (embedded Olric cluster across the app processes; opt-in for multi-VM regions where rate-limit and connection-cap state must be shared). |
| `CACHE_OLRIC_BIND_ADDR` | no | `127.0.0.1` | Olric protocol bind. |
| `CACHE_OLRIC_BIND_PORT` | no | `3320` | Olric protocol port. |
| `CACHE_OLRIC_MEMBERLIST_ADDR` | no | `127.0.0.1` | Memberlist (gossip) bind. |
| `CACHE_OLRIC_MEMBERLIST_PORT` | no | `3322` | Memberlist port. |
| `CACHE_OLRIC_PEERS` | no | (empty) | Comma-separated host:port peers; empty means cluster of one. |

## HTTP API surface (Phase 0)

| Path | Status | Notes |
|---|---|---|
| `GET /v1/healthz` | 200 | `{"status":"ok","version":"v1","commit":"<sha>"}`. Sets `X-API-Version: v1`. |
| `GET /metrics` | 200 | Prometheus scrape target. Versionless on purpose. |
| any non-`/v1/` path | 404 | Enforced by chi `r.Mount("/v1", ...)` + default `NotFound`. |

Phase 1 adds `/v1/auth/*`, `/v1/storage/*`, `/v1/leaderboards/*`, and the
`tenant` middleware that guards all of them.

## Dashboard auth

The dashboard is a separate operator surface under `/v1/dashboard`, not part
of the player auth API. It uses platform-scoped `dashboard_users` with bcrypt
password hashes, opaque DB-backed `dashboard_sessions`, and CSRF tokens on
state-changing forms. A `dashboard_memberships` table maps dashboard users to
tenants with `owner`, `admin`, or `member` roles; API-key management requires
`admin` or stronger. Platform admins can see every tenant.

Fresh installs are bootstrapped with a one-time token generated at server
startup when `dashboard_users` is empty. The token is written to
`DASHBOARD_BOOTSTRAP_TOKEN_FILE` (if set) or stderr, and accepted only by
`/v1/dashboard/setup`. Operators paste the token into the setup form — it is
never pre-filled from a URL query parameter to keep it out of access logs and
browser history. Once the first platform admin is created, setup returns 410
and all access goes through email/password login.

## Reverse-proxy IP trust

Dashboard sessions record the client IP for auditing. `clientIP()` reads
`CF-Connecting-IP` first (Cloudflare), then `X-Real-IP` (nginx/HAProxy), then
falls back to `RemoteAddr`. **This is only safe when the reverse proxy strips
these headers from untrusted client requests on ingress.** If the proxy does
not strip them, a client can spoof any IP address in the session audit record.
The compose files in this repo sit behind Cloudflare (ops) or a direct bind
(dev); both configurations strip `CF-Connecting-IP` at the edge or it is
absent entirely.

## Tenant isolation (Phase 1)

Tenant isolation is enforced at two layers; either alone is sufficient to
block cross-tenant access, and the test suite exercises both.

**Layer 1 — `internal/tenant` HTTP middleware.** Extracts the
`Authorization: Bearer <key>` header, hashes the token with SHA-256, calls a
`Lookup` against `api_keys` (returning `ErrUnknownKey` on no match), and
maps results to status codes:

- missing header / non-Bearer scheme / empty token / unknown key → **401**
- recognised but revoked key → **403**
- unexpected lookup error → **500**

On success the middleware injects `tenant_id` (and optionally `project_id`)
into the request context via `internal/db.WithTenant` / `WithProject`. No
external header (e.g. `X-Tenant-Id`) is ever consulted; the resolver is the
sole source of truth.

**Layer 2 — Postgres Row-Level Security.** Migration `0009_rls.up.sql`
enables RLS on every tenant-scoped table with a single policy:
`tenant_id = current_setting('app.tenant_id', true)::bigint`. The `, true`
flag makes a missing GUC return NULL, so the policy fails closed (no rows
visible) when application code forgets to scope.

The application sets the GUC via `db.Q(ctx, fn)`, which opens a
transaction, executes `SET LOCAL app.tenant_id = '<id>'`, then runs the
caller's closure. The setting is scoped to the tx and dropped on commit
or rollback.

**Why both.** Middleware filters at the API boundary; RLS catches anything
that bypasses the middleware (a future internal job, a developer using
`pool.Exec` directly, an SDK bug). Defence in depth — and the bypass policy
on `api_keys` (`api_keys_bootstrap` in migration 0010) is the single
deliberate exception, scoped to `SELECT` only when no GUC is set so the
resolver itself can run.

**Role separation.** The runtime connects as `ggscale_app` (created in
`0007_audit_log`, granted in `0010_app_role_grants`). It is not a
superuser, so RLS applies. `audit_log` keeps a narrower grant
(`INSERT`, `SELECT` only) so even a compromised app session cannot
rewrite history.

## Auth headers — `Authorization` vs `X-Session-Token`

Every authenticated request carries two distinct identities and we split
them across two headers:

| Header | Carries | Issued to | Lifetime |
|---|---|---|---|
| `Authorization: Bearer <api_key>` | The **tenant** identity (the game studio) | Tenant operator at project create time | Long-lived; rotated only on revoke |
| `X-Session-Token: <jwt>` | The **end-user** identity (the player) | `/v1/auth/*` after sign-up / login / anonymous / custom-token | 15 minutes (refresh via `/v1/auth/refresh`) |

This is intentionally inverted from the convention some HTTP guides
recommend (where the user JWT lives in `Authorization`). The reasoning:

- **SDK ergonomics.** Game SDKs hard-code the api_key in the
  `Authorization` header at construction and rotate the player JWT in
  `X-Session-Token` per session. Players signing in/out doesn't disturb
  the api_key plumbing.
- **Independent middleware tiers.** `internal/tenant` (Layer 1 above)
  reads `Authorization` and runs *before* rate-limiting. The end-user
  middleware (`internal/enduser`) reads `X-Session-Token` and runs only
  on routes that need the player identity. Cleanly separating the
  headers keeps each middleware single-purpose.
- **Mirrors Firebase / Supabase**, where the SDK ships the project
  api_key in one slot and the user JWT in another.

A stolen `X-Session-Token` cannot be replayed under a different tenant's
api_key: `enduser.New` asserts `claims.TenantID == ctx.tenant`.

## Observability

Prometheus scrapes the server's `/metrics` endpoint every 15 s. The
`ggscale_http_requests_by_version_total` counter (label: `version`) is
incremented by `version.Middleware` for every request inside the `/v1`
subtree. Standard Go runtime + process collectors are also registered.
Prometheus's own UI at `http://localhost:9090/graph` is the dev surface
for poking at counters; a full Grafana + Loki + Promtail dashboarding
stack is deferred to v1.1 (see `ROADMAP.md`).

Logs are structured JSON via `slog`, written to stdout. In dev,
`docker compose logs -f ggscale-server` is the log surface.

## Database migrations

- Forward-only SQL files in `db/migrations/`, named `NNNN_<name>.up.sql` /
  `.down.sql`.
- `0001_init.up.sql` is extensions-only (`pgcrypto`, `citext`). Phase 1 owns
  the first table-creating migration.
- `make migrate` runs the `migrate/migrate:v4.19.1` image against the
  compose postgres. The same image is also wired as a compose init
  container, so `make up` applies pending migrations before
  `ggscale-server` boots.
- `internal/migrate.Runner` is the in-process API used by integration
  tests (build tag `integration`, runs `testcontainers-go` against
  `postgres:17`).

## CI

`.github/workflows/ci.yml` runs three jobs on `ubuntu-24.04`. Workflow-level
permissions default to `contents: read`; jobs elevate scope only when needed.
Action versions are pinned to commit SHAs.

1. **lint-test** — `golangci-lint` (v2.11.4 with errcheck/govet/staticcheck/
   unused/gosec/gocritic/revive), `go test -race`, `govulncheck` (pinned).
2. **docker-build** — multi-stage Docker build. Image is exported as a
   tarball and uploaded as a workflow artifact (1-day retention) so the
   `e2e` job can consume it. Runs with `contents: read` only — no GHCR
   credentials.
3. **e2e** — depends on `docker-build`; loads the artifact image, starts the
   full compose stack including the k8s profile, installs Agones, runs
   `go test -tags=e2e ./e2e/...` (the healthz suite + the Agones allocation
   smoke). On failure, compose logs are captured as an artifact.

A fourth job, `docker-publish` (push image to GHCR on main), is wired and
commented out. We'll re-enable it when Phase 0 testing+building proves
stable. See `.github/workflows/ci.yml`.

`.github/dependabot.yml` opens weekly dependency-bump PRs for Go modules,
GitHub Actions, and Docker base images.

## Lifecycle architecture (forward-looking)

The Phase 0 architecture above is the substrate for **three** product
audiences, not one — the v1.0 indie / mid-tier BaaS audience, the v1.9+
B2B sunset-services audience, and the v1.9+ B2C white-label community
hosting audience. The strategic framing lives in [`LIFECYCLE.md`](LIFECYCLE.md).
This subsection records the architectural commitment: **no v1.9+ SKU
requires a structural change to the v1.0 platform.**

```
                                   ┌──────────────────────────────────────────────────────┐
   live-service indie  ───────►    │                                                      │
   (audience #1, v1.0)             │  ggscale control plane (multi-tenant, v1.0)          │
                                   │  ─ tenant middleware (api-key → tenant_id)           │
   sunset-port publisher ──────►   │  ─ Postgres + RLS, cache.Store (memory|olric),       │
   (audience #2, v1.9+)            │    K3s + Agones, pion/turn                           │
                                   │  ─ Cloudflare LB + origin-pool routing, Stripe, GDPR │
                                   │                                                      │
   white-label community ──────►   │  every "customer" is a tenant; the SKU determines    │
   (audience #3, v1.9+)            │  who pays, what's branded, and where royalties flow  │
                                   └──────────────────────────────────────────────────────┘
```

What each audience consumes (and where it ships):

| Audience | Tenant origin | Branding | Billing entity | Primary substrate |
|---|---|---|---|---|
| #1 — indie / mid-tier launch | Studio signs up at `ggscale.io` | ggscale-managed under studio's namespace | Studio (PAYG / Premium / self-host) | Multi-tenant control plane + Agones fleet + STUN/TURN relay (v1.0 Phase 1–2). |
| #2 — sunset port (B2B) | Publisher engagement; ggscale provisions tenant after porting work | Publisher brand on a self-host bundle the community runs | Publisher (fixed-fee engagement) | Same `docker-compose.yml` + `bw-ops` Ansible the v1.0 self-host audience uses. The published v1.0 self-host migration guide is the deliverable's runtime substrate. |
| #3 — white-label community hosting (B2C) | ggscale provisions per-rental tenants under a publisher's IP | Publisher brand on a ggscale-hosted subdomain | Players (consumer subscription) → royalty to publisher | Same multi-tenant control plane + Agones fleet + Cloudflare LB hostname routing as audience #1, with a consumer storefront UX layered on top. |

The v1.0 architectural decisions that make this possible — and that
v1.9+ planning must therefore not regress on — are:

- **Tenant isolation as a structural invariant.** Every tenant-scoped
  table carries `tenant_id`; RLS enforces it at the DB layer; the tenant
  middleware enforces it at the API layer. A "white-label rental" is
  just a tenant; a "ported sunset title" is just a tenant. No special
  per-SKU plumbing.
- **Multi-tenancy on shared schema, not per-tenant DBs (default).** Cheap
  per-tenant overhead is what makes the B2C white-label storefront
  economically viable — provisioning a new rental cannot require a new
  Postgres cluster or it ruins the unit economics.
- **Agones fleet manager as the orchestration boundary.** A sunset
  title's game-server image is a `Fleet` CRD just like a live title's;
  Agones lifecycle (`Allocator` API, `RollingUpdate` strategy, drain on
  `SDK.Shutdown()`) works identically.
- **Stripe metering covers consumer subscriptions natively.** The same
  Prometheus-driven CCU gauge + in-memory daily accumulator + per-day
  `usage_aggregates` row pipeline (Phase 3 design — see `mvp.md` §
  "Metering and billing pipeline") that bills PAYG indie tenants will
  bill consumer rentals — the difference is the Stripe product
  configuration, not a code path.
- **Self-host parity by construction.** The published `docker-compose.yml`
  is what the v1.0 self-host audience uses, what CI runs on every PR,
  *and* what `ggscale Sunset` engagements hand to publishers as the
  "community can run this themselves" deliverable. The Phase 0 commitment
  to "what runs in CI is what the public self-host stack runs" is the
  technical foundation of the entire B2B sunset value proposition.

What v1.9+ does add (recorded here so this doc can grow into it without
surprise): a consumer-storefront frontend (browse sunset titles, rent an
instance, manage server settings) and a publisher royalty dashboard.
Both are application-layer additions on top of the v1.0 control plane —
new UI, no new substrate.

## VM-based single-tenant provisioning (forward-looking)

A second orchestration substrate planned alongside the K3s+Agones pod
fleet. **Strictly sequenced after Kubernetes is in production** — this
ships only once the v1.0 Agones fleet and the v1.1 dynamic K3s
autoscaler are working. The pod fleet remains the default for
multi-tenant indie / mid-tier workloads; VM provisioning is the
Premium-tier escape hatch.

The substrate: Packer-built VM images provisioned via OVH Public
Cloud's OpenStack API (Nova / Neutron / Cinder), one tenant per VM by
construction. Because OVH exposes vanilla OpenStack, the same
provisioner extends to BYO-OpenStack as a follow-on for tenants with
their own private cloud.

What it unlocks:

- **Windows-only game servers** that the Linux K3s node story can't host.
- **High-perf workloads** that need kernel tuning, CPU pinning, or
  NUMA layouts containers don't comfortably expose.
- **Heavy per-tenant customization** (kernel modules, anti-cheat
  drivers, exotic networking) that doesn't belong on a shared pod
  substrate.

Architectural commitment: the control plane treats "VM instance" and
"Agones GameServer" as two implementations of the same allocation
primitive. Tenant isolation, observability scrape, and the metering
pipeline are unchanged — the new substrate plugs into the existing
boundaries, not around them. Full scope and sequencing live in
[`ROADMAP.md`](ROADMAP.md) under v1.4+.

## Related documents

- [`LIFECYCLE.md`](LIFECYCLE.md) — canonical product strategy: three
  audiences, the regulatory tailwind, the v1.0 vs. v1.9+ split, and how
  this architecture serves all three without rework.
- [`HA.md`](HA.md) — high-availability runbook: current HA posture,
  blue/green and Agones rolling-deploy procedures, failure-mode recovery
  for production, and the v1.1 path to closing Postgres + cache + K3s
  SPOFs.
- [`mvp.md`](mvp.md) — strategic plan, all phases.
- [`ROADMAP.md`](ROADMAP.md) — public versioned roadmap (v1.0 → v2.x).
- [`RUNBOOK.md`](RUNBOOK.md) — failure-mode recovery for the dev stack.
- [`RATELIMIT.md`](RATELIMIT.md) — HTTP rate-limit and WS connection-cap
  contract: tier defaults, response shapes, key formats, SDK guidance.
