# ggscale — architecture

This document tracks what ships in-tree today: the multi-tenant HTTP +
WebSocket API under `/v1/`, the pluggable game-server fleet (Docker, Agones,
or out-of-tree plugin subprocesses), the realtime hub and ticket-driven
matchmaker, the embedded TURN relay, Postgres + RLS, pluggable mail, and
observability on `/metrics`. Strategic sequencing lives in [`mvp.md`](mvp.md).

## Deployment topology

There are four compose scenarios, each in its own file:

- **basic dev** (default `make up`, root `docker-compose.yml`) — Postgres, migrate,
  `ggscale-server`, MailHog. Dashboard on host port **3001** (mapped to the server).
  No Prometheus sidecar in this file; scrape `/metrics` from your own collector or use
  `make up-full` (`compose/full.yml`) for bundled Prometheus.
- **fleet/docker** (`make up-fleet-docker`, `compose/fleet-docker.yml`) — basic dev plus
  `FLEET_BACKEND=docker` and a `/var/run/docker.sock` mount so `ggscale-server`
  allocates game-server containers on demand.
- **fleet/agones** (`make up-fleet-agones`, `compose/fleet-agones.yml`) — basic dev plus
  k3s (single-node, `--network=host`) and an Agones install job. Required for the
  Agones e2e smoke test.
- **full** (`make up-full`, `compose/full.yml`) — fleet/docker plus Prometheus and
  stripe-mock for the contributor environment.

```
                   ┌──────────────────────────────────────────────┐
   game client     │ ggscale-server :8080  (Go, chi router)       │
   ──────────────► │  /v1/healthz · /v1/auth · /v1/storage · …    │
                   │  /v1/ws            (realtime Hub)            │
                   │  /v1/matchmaker/*  (ticket queue + worker)   │
                   │  /v1/relay/*       (TURN-REST credentials)   │
                   │  /v1/dashboard     (HTMX + templ)            │
                   │  /metrics                                    │
                   │  + cache.Store (memory or olric)             │
                   │  + fleet.Manager → Backend                   │
                   └──┬──────────────────────────────┬────────────┘
                      │                              │
                postgres:5432                  fleet backend
                (migrate init,                 ──────────────
                 LISTEN/NOTIFY                 docker | agones |
                 wakeup for                    plugin:<name>
                 matchmaker)
                                       (also: pion/turn :3478 UDP
                                        when RELAY_PUBLIC_IP set)
```

## Simple-stack services (`docker-compose.yml`)

| Service | Image | Port | Role |
|---|---|---|---|
| `postgres` | `postgres:17` | 5432 | Primary DB. `pg_isready` healthcheck. |
| `migrate` | `migrate/migrate:v4.19.1` | — | One-shot init; applies `db/migrations/*.up.sql` before the server starts. |
| `ggscale-server` | build `Dockerfile` | 8080, 3001→8080 | `/v1/*` HTTP API, `/metrics`, HTMX dashboard at `/v1/dashboard/*`. |
| `mailhog` | `mailhog/mailhog:v1.0.1` | 1025 / 8025 | SMTP sink + web UI for auth/profile verification mail in dev. |

**Full dev stack** (`compose/full.yml`, `make up-full`) adds Prometheus and
Stripe mock on top of the Docker fleet backend — see that file for the extended service matrix.

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
| `agones-install` | `bitnami/kubectl:1.30` | One-shot job that applies `infra/k8s/agones-install.yaml` (pinned Agones v1.42.0) and waits for the controller + allocator to roll out. |

The Agones install manifest is committed verbatim under `infra/k8s/agones-install.yaml`
(top-of-file comment records the source URL and version). Bumping Agones is a
re-fetch and a PR.

## Why `--network=host` for k3s

The MVP requires that game-server UDP `hostPort`s assigned by Agones be
reachable directly from the host OS — no Docker bridge, no port-forward proxy.
That's the `--network=host` contract. It's the substrate the Agones e2e
smoke test exercises (`TestAgonesAllocation_AssignsHostPort_ReachableViaUDP`):

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
a Linux VM). For `make up-fleet-agones` on macOS, run **Colima** with host networking
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
| `FLEET_BACKEND` | no | `docker` | Fleet backend selector: `docker`, `agones`, or `plugin:<name>`. |
| `FLEET_REGION` | no | `local` | Region label persisted on every allocation. |
| `FLEET_PLUGIN_DIR` | no | `/etc/ggscale/plugins` | Scanned for `ggscale-fleet-<name>` plugin binaries when `FLEET_BACKEND` starts with `plugin:`. |
| `DOCKER_GAMESERVER_IMAGE` | docker only | (empty) | Image to run for each allocation. Empty disables the fleet (matchmaker rejects Allocate). |
| `DOCKER_GAMESERVER_PORT` | no | `7777` | Container port the health probe targets. |
| `DOCKER_PROBE_TYPE` | no | `tcp` | `tcp`, `http`, or empty to disable readiness probing. |
| `DOCKER_PROBE_PATH` | no | `/healthz` | HTTP probe path (when `DOCKER_PROBE_TYPE=http`). |
| `DOCKER_HOST` | no | (empty) | Override the docker daemon endpoint. Honoured by the upstream SDK directly. |
| `AGONES_NAMESPACE` | no | `default` | Namespace the `GameServerAllocation` CR is created in. |
| `AGONES_FLEET_NAME` | no | (empty) | Optional `agones.dev/fleet=<name>` selector. |
| `AGONES_SELECTOR_LABELS` | no | (empty) | Additional `k=v,…` label selector. |
| `AGONES_KUBECONFIG` | no | (empty) | Path to kubeconfig. Empty tries in-cluster, then `~/.kube/config`. |
| `REALTIME_MAX_PER_TENANT` | no | `0` | Concurrent `/v1/ws` connections per tenant; 0 disables the cap. |
| `MATCHMAKER_BUCKET_SIZE` | no | `1` | Tickets per allocated match in a `(tenant, project, region, game_mode)` bucket. |
| `MATCHMAKER_INTERVAL` | no | `5s` | Fallback scan cadence. The hot path is Postgres LISTEN/NOTIFY; this ticker catches gaps during a listener reconnect. |
| `RELAY_PUBLIC_IP` | no | (empty) | Public address advertised to TURN peers. Setting this + `RELAY_SHARED_SECRET` boots the UDP listener. |
| `RELAY_BIND_ADDR` | no | `0.0.0.0` | UDP bind host. |
| `RELAY_UDP_PORT` | no | `3478` | UDP bind port. |
| `RELAY_REALM` | no | `ggscale` | TURN realm string. |
| `RELAY_SHARED_SECRET` | no | (empty) | HMAC secret backing the TURN-REST issuer. Supports `RELAY_SHARED_SECRET_FILE`. |
| `RELAY_CRED_TTL` | no | `5m` | Validity window of issued credentials. |

## HTTP API surface (`/v1/`)

All product routes mount under `/v1/`; anything else returns **404**.

| Area | Paths (summary) | Auth |
|---|---|---|
| Health | `GET /v1/healthz` | Public |
| Metrics | `GET /metrics` | Public (versionless by design) |
| Auth | `POST /v1/auth/signup`, `/verify`, `/login`, `/refresh`, `/logout`, `/anonymous`, `/custom-token` | Tenant API key (`Authorization: Bearer`); bcrypt + JWT session |
| Storage | `PUT/GET/DELETE /v1/storage/objects/...`, list | API key + `X-Session-Token` |
| Leaderboards | `POST .../scores`, `GET .../top`, `GET .../around-me` | Submit: secret key + session; reads: key + session |
| Friends | `POST/GET/DELETE /v1/friends/...` | API key + session |
| Profile | `GET/PATCH /v1/profile/` | API key + session |
| Realtime | `GET /v1/ws` (WebSocket upgrade; heartbeat, presence, `match_ready` push) | API key + session |
| Matchmaker | `POST/GET/DELETE /v1/matchmaker/tickets[/{id}]` | API key + session |
| Relay | `POST /v1/relay/credentials` (TURN-REST credential issue) | API key + session |
| Dashboard | `/v1/dashboard/*` | Separate platform session (bcrypt users, CSRF) |

**Tenant middleware** (`internal/tenant`) runs on the API-key group before rate limiting
and resolves `Authorization: Bearer` → `tenant_id` / optional `project_id`.

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
`RemoteAddr` by default and ignores forwarded headers. To record a proxy
supplied address, set `TRUSTED_PROXY_HEADER` (for example
`CF-Connecting-IP`) and `TRUSTED_PROXY_CIDRS` to the CIDR ranges of the
reverse proxies allowed to set that header. Headers from any other peer are
ignored, so direct clients cannot spoof dashboard session or audit IPs.

## Tenant isolation

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
| `X-Session-Token: <jwt>` | The **player** identity (the player) | `/v1/auth/*` after sign-up / login / anonymous / custom-token | 15 minutes (refresh via `/v1/auth/refresh`) |

This is intentionally inverted from the convention some HTTP guides
recommend (where the user JWT lives in `Authorization`). The reasoning:

- **SDK ergonomics.** Game SDKs hard-code the api_key in the
  `Authorization` header at construction and rotate the player JWT in
  `X-Session-Token` per session. Players signing in/out doesn't disturb
  the api_key plumbing.
- **Independent middleware tiers.** `internal/tenant` (Layer 1 above)
  reads `Authorization` and runs *before* rate-limiting. The player
  middleware (`internal/playerauth`) reads `X-Session-Token` and runs only
  on routes that need the player identity. Cleanly separating the
  headers keeps each middleware single-purpose.
- **Mirrors Firebase / Supabase**, where the SDK ships the project
  api_key in one slot and the user JWT in another.

A stolen `X-Session-Token` cannot be replayed under a different tenant's
api_key: `playerauth.New` asserts `claims.TenantID == ctx.tenant`.

## Game-server fleet

`fleet.Manager` (in `internal/fleet/manager.go`) is the single allocator
the matchmaker and any future caller talk to. It owns persistence in
`game_server_allocations` (migration `0019`), retry/backoff, and a
backend-agnostic state machine: pending → allocating → ready → allocated
→ shutdown/failed. The backend itself is one Go interface:

```go
type Backend interface {
    Name() string
    Allocate(ctx, AllocationRequest)         (*Allocation, error)
    Deallocate(ctx, AllocationID, ref)       error
    Status(ctx, AllocationID, ref)           (Status, error)
    Watch(ctx, AllocationID, ref)            (<-chan StatusUpdate, error)
    HealthCheck(ctx)                         error
}
```

Three implementations ship in-tree:

- **Docker** (`internal/fleet/docker`) — talks to the local daemon via the
  upstream Go SDK, runs one container per allocation, attaches
  `ggscale.tenant_id` / `project_id` / `region` labels, dynamically maps
  the configured port, and translates Docker events (`Start`,
  `HealthStatusHealthy`, `Die`, `OOM`) into `fleet.StatusUpdate` frames.
  Cold start under three seconds in the e2e test.
- **Agones** (`internal/fleet/agones`) — uses the typed Kubernetes
  clientset to create a `GameServerAllocation` CR and `Watch` against the
  resulting `GameServer`, mapping Agones lifecycle states through the same
  enum.
- **Plugin shim** (`internal/fleet/plugin`) — out-of-tree backends ship as
  separate binaries under `/etc/ggscale/plugins/` and talk to the host
  over gRPC under `hashicorp/go-plugin` with AutoMTLS (handshake cookie
  `GGSCALE_FLEET_PLUGIN` / `ggscale-v1`, protocol version 1). A
  supervisor watches the subprocess, restarts up to three consecutive
  crashes, and force-kills plugins that stop answering `Ping`. An
  optional `<binary>.manifest.toml` sidecar carries the plugin's
  declared `name` / `version` / `protocol_version`. Author guide:
  [`fleet-plugins.md`](fleet-plugins.md).

Selection is operator-controlled at startup via `FLEET_BACKEND`
(`docker`, `agones`, `plugin:<name>`). The Manager is unaware of which
side it's calling; tenant context flows through `db.WithTenant` so
Postgres-backed allocations still receive `app.tenant_id` for RLS.

## Realtime WebSocket hub

`internal/realtime` is the WebSocket fan-out for ggscale. Players open
one persistent connection to `/v1/ws` after authenticating; the server
registers it in `realtime.Hub` keyed by `(tenant_id, player_id)` so
any backend goroutine — matchmaker, presence, future lobby/chat — can
push a message at a specific player without knowing how they're
connected.

Key contracts:

- **Transport.** `coder/websocket`. The Hub holds a transport-agnostic
  `Writer` interface; the production wrapper writes JSON text frames.
- **Heartbeat.** Server-initiated `Ping` every 30 s (configurable). A
  missed `Pong` closes the connection; the read loop exits, releasing
  the slot.
- **Per-tenant CCU cap.** When `REALTIME_MAX_PER_TENANT > 0`, the
  handler calls `cache.Store.AcquireSlot("realtime:tenant:<id>", ...)`
  before upgrading and `ReleaseSlot` on disconnect. The Olric cache
  backend shares the counter across multi-VM regions.
- **Send.** Matchmaker and other callers invoke `Hub.Send(ctx,
  tenantID, playerID, Message{Type:"match_ready", Payload: ...})`.
  Returns `ErrNotConnected` when the player is offline; callers decide
  whether to retry or fall back to polling.

The Hub itself is concurrency-safe and transport-agnostic — unit tests
inject a fake `Writer`; the integration test in `internal/matchmaker`
exercises the real upgrade path against an httptest server.

## Matchmaker

The matchmaker (`internal/matchmaker`) turns player "find me a match"
requests into game-server allocations. Players POST a ticket to
`/v1/matchmaker/tickets`; a background worker batches tickets into
buckets keyed by `(tenant_id, project_id, region, game_mode)` and, once
a bucket fills, calls `fleet.Manager.Allocate` exactly once. Each
matched player gets a `match_ready` envelope pushed through the realtime
hub with the live server's address.

Schema lives in migration `0019_fleet_allocations` (the allocation table)
and `0020_matchmaking_tickets` (the queue), both RLS-isolated by
`tenant_id`. The queue uses an `AFTER INSERT` trigger added in
`0021_matchmaker_notify` that emits `NOTIFY matchmaker_ticket` with a
JSON `(tenant, project, region, game_mode)` payload — the worker
subscribes via Postgres LISTEN and wakes event-driven within
milliseconds of a ticket landing, instead of polling.

The worker loop converges three event sources:

1. **Listener events.** Hot path; one bucket processed per `NOTIFY`.
2. **Fallback ticker** (`MATCHMAKER_INTERVAL`, default 5 s). Catches
   tickets that arrive during a listener reconnect gap or when the
   queue backend doesn't implement the optional `Listener` interface
   (e.g. the in-memory queue used in tests).
3. **`ctx.Done`** for graceful shutdown.

Bucket processing uses `FOR UPDATE SKIP LOCKED` (`PopMatchmakerBucket`)
so multiple ggscale-server processes can run workers safely against the
same Postgres without double-popping. The HTTP `GET
/v1/matchmaker/tickets/{id}` path is the authoritative source of truth
for clients — SDKs subscribe to the WebSocket push but should poll on
reconnect to recover from any missed `match_ready` delivery.

## TURN relay

`internal/relay` wraps `pion/turn/v3` with ggscale's tenant + player
identity model. Authentication follows the standard TURN-REST shape:
the username encodes `<expires>:<tenant_id>:<player_id>` and the
password is a base64 HMAC-SHA1 of the username under
`RELAY_SHARED_SECRET`. An authenticated player calls `POST
/v1/relay/credentials` and receives `{username, password, ttl, realm,
urls}`.

The HTTP credential endpoint and the UDP TURN listener are gated
independently:

- **Issuer only** — `RELAY_SHARED_SECRET` set, `RELAY_PUBLIC_IP` empty.
  Useful for issuing credentials to a relay you host elsewhere.
- **Issuer + embedded server** — both set. The TURN listener boots at
  `RELAY_BIND_ADDR:RELAY_UDP_PORT` (default `0.0.0.0:3478`) with a
  `RelayAddressGeneratorStatic` pointing at `RELAY_PUBLIC_IP`. The
  AuthHandler recomputes the HMAC password and runs it through
  `pion.GenerateAuthKey`, refusing requests that don't match the
  issuer's view of the realm.

## Observability

The server exposes Prometheus metrics at **`/metrics`**: HTTP latency histograms, error
counter, DB query timings (pgx tracer), cache op counters, and rate-limit throttle
counter. Wire your own Prometheus scrape config, or use the full dev compose overlay
for a local Prometheus UI.

**Structured logs:** `slog` JSON to stdout. `middleware.NewContextHandler` enriches records
with `tenant_id`, `project_id`, and `request_id` when present.

Grafana + Loki + Promtail are deferred to v1.1 per milestone notes.

## Database migrations

- Forward-only SQL files in `db/migrations/`, named `NNNN_<name>.up.sql` /
  `.down.sql`. Current head is `0021_matchmaker_notify`.
- `0001_init.up.sql` is extensions-only (`pgcrypto`, `citext`); the first
  table-creating migration is `0002_tenants`.
- Notable trigger-bearing migrations: `0021_matchmaker_notify` adds an
  `AFTER INSERT` trigger on `matchmaking_tickets` that emits a
  `pg_notify('matchmaker_ticket', json_build_object(...))` payload so
  the matchmaker worker wakes event-driven instead of polling.
- `make migrate` runs the `migrate/migrate:v4.19.1` image against the
  compose postgres. The same image is also wired as a compose init
  container, so `make up` applies pending migrations before
  `ggscale-server` boots.
- `internal/migrate.Runner` is the in-process API used by integration
  tests (build tag `integration`, runs `testcontainers-go` against
  `postgres:17`).

## CI

`.github/workflows/ci.yml` runs jobs on `ubuntu-24.04`. Workflow-level permissions
default to `contents: read`; jobs elevate scope only when needed. Action versions are
pinned to commit SHAs.

1. **lint-test** — `golangci-lint` (v2.11.4 with errcheck/govet/staticcheck/
   unused/gosec/gocritic/revive), `go test -race`, integration tests
   (`go test -race -tags=integration`, Docker-backed testcontainers), `govulncheck` (pinned).
2. **docker-build** — multi-stage Docker build. Image is exported as a
   tarball and uploaded as a workflow artifact (1-day retention) so the
   `e2e` job can consume it. Runs with `contents: read` only — no GHCR
   credentials.
3. **e2e** — depends on `docker-build`; loads the artifact image, starts compose
   (lite stack + k8s profile), installs Agones, runs `go test -tags=e2e ./tests/e2e/...`
   (healthz against ggscale/MailHog/dashboard; Agones smoke skips when no kubeconfig).
   On failure, compose logs are captured as an artifact.

A fourth job, `docker-publish` (push image to GHCR on main), is wired and
commented out. We'll re-enable it when the test + build pipeline proves
stable. See `.github/workflows/ci.yml`.

`.github/dependabot.yml` opens weekly dependency-bump PRs for Go modules,
GitHub Actions, and Docker base images.

## Lifecycle architecture (three audiences, one substrate)

The architecture above is the substrate for **three** product audiences,
not one — the v1.0 indie / mid-tier BaaS audience (live today), the
v1.9+ B2B sunset-services audience, and the v1.9+ B2C white-label
community hosting audience. The strategic framing lives in
[`LIFECYCLE.md`](LIFECYCLE.md). This subsection records the architectural
commitment: **no v1.9+ SKU requires a structural change to the v1.0
platform.**

```
                                   ┌──────────────────────────────────────────────────────┐
   live-service indie  ───────►    │                                                      │
   (audience #1, v1.0)             │  ggscale control plane (multi-tenant, shipped)       │
                                   │  ─ tenant middleware (api-key → tenant_id)           │
   sunset-port publisher ──────►   │  ─ Postgres + RLS, cache.Store (memory|olric)        │
   (audience #2, v1.9+)            │  ─ fleet.Backend: Docker | Agones | plugin           │
                                   │  ─ realtime Hub, matchmaker (LISTEN/NOTIFY)          │
   white-label community ──────►   │  ─ pion/turn relay, Cloudflare LB, Stripe, GDPR      │
   (audience #3, v1.9+)            │                                                      │
                                   │  every "customer" is a tenant; the SKU determines    │
                                   │  who pays, what's branded, and where royalties flow  │
                                   └──────────────────────────────────────────────────────┘
```

What each audience consumes (and where it ships):

| Audience | Tenant origin | Branding | Billing entity | Primary substrate |
|---|---|---|---|---|
| #1 — indie / mid-tier launch | Studio signs up at `ggscale.io` | ggscale-managed under studio's namespace | Studio (PAYG / Premium / self-host) | Multi-tenant control plane + pluggable fleet (Docker/Agones/plugin) + TURN relay — all shipped. |
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
- **Fleet `Backend` interface as the orchestration boundary.** A sunset
  title's game-server image is a Docker container, an Agones `Fleet`
  CRD, or a plugin-managed VM — all addressed through the same six-method
  `Backend` contract. The matchmaker doesn't know which side it's
  calling. Swapping or adding orchestration substrates doesn't touch the
  control plane.
- **Stripe metering covers consumer subscriptions natively.** The same
  Prometheus-driven CCU gauge + in-memory daily accumulator + per-day
  `usage_aggregates` row pipeline (Phase 3 design — see `mvp.md` §
  "Metering and billing pipeline") that bills PAYG indie tenants will
  bill consumer rentals — the difference is the Stripe product
  configuration, not a code path.
- **Self-host parity by construction.** The published `docker-compose.yml`
  is what the v1.0 self-host audience uses, what CI runs on every PR,
  *and* what `ggscale Sunset` engagements hand to publishers as the
  "community can run this themselves" deliverable. The structural
  commitment that "what runs in CI is what the public self-host stack
  runs" is the technical foundation of the entire B2B sunset value
  proposition.

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
