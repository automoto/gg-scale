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
                   └─────────────┬──────────────┬───────────┘
                                 │              │
                       postgres:5432       valkey:6379
                       (init via            (cache,
                       migrate init         matchmaking
                       container)           pools — Phase 1+)
```

## Lite-profile services

| Service | Image | Port | Role |
|---|---|---|---|
| `ggscale-server` | local `ggscale-server:dev` | 8080 | Control-plane HTTP API (chi router, slog JSON, prometheus `/metrics`). |
| `postgres` | `postgres:17` | 5432 | Primary DB. `pg_isready` healthcheck. |
| `valkey` | `valkey/valkey:8` | 6379 | OSS Redis fork. Phase 0 doesn't yet read/write it; Phase 1 adds matchmaking/presence. |
| `migrate` | `migrate/migrate:v4.19.1` | — | One-shot init container; applies `db/migrations/*.up.sql` against postgres before `ggscale-server` starts. |
| `prometheus` | `prom/prometheus:v3.1.0` | 9090 | Scrapes `ggscale-server:8080/metrics`. Browse counters at `http://localhost:9090/graph`. |
| `mailhog` | `mailhog/mailhog:v1.0.1` | 1025 / 8025 | SMTP sink + web UI for capturing signup/verify mail (Phase 1+). |
| `stripe-mock` | `stripe/stripe-mock:v0.197.0` | 12111 / 12112 | Local Stripe API mock for the billing flows that land in Phase 3. Pre-wired now to avoid later compose churn. |
| `dashboard-stub` | `nginx:1.27-alpine` | 3001 | One-line static placeholder. Phase 1 replaces with the real read-only ops dashboard. |

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
| `VALKEY_ADDR` | no | `localhost:6379` | Valkey/Redis address. |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `ENV` | no | `dev` | `dev`, `staging`, `prod`. |

## HTTP API surface (Phase 0)

| Path | Status | Notes |
|---|---|---|
| `GET /v1/healthz` | 200 | `{"status":"ok","version":"v1","commit":"<sha>"}`. Sets `X-API-Version: v1`. |
| `GET /metrics` | 200 | Prometheus scrape target. Versionless on purpose. |
| any non-`/v1/` path | 404 | Enforced by chi `r.Mount("/v1", ...)` + default `NotFound`. |

Phase 1 adds `/v1/auth/*`, `/v1/storage/*`, `/v1/leaderboards/*`, and the
`tenant` middleware that guards all of them.

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

## Related documents

- [`mvp.md`](mvp.md) — strategic plan, all phases.
- [`ROADMAP.md`](ROADMAP.md) — public versioned roadmap (v1.0 → v2.x).
- [`RUNBOOK.md`](RUNBOOK.md) — failure-mode recovery for the dev stack.
