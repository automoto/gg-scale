# ggscale

Open-source, self-hostable backend for multiplayer games. One Go binary and a Postgres database give you player accounts, saves, leaderboards, social features, matchmaking, and a game-server fleet. Run it on a single VPS, keep your data, and keep the game online after the studio that built it has moved on.

## What it is

`ggscale-server` is a single Go binary. Point it at a Postgres URL and it serves a multi-tenant HTTP + WebSocket API under `/v1/`, an admin dashboard, and an optional hosted player site. A second compose file adds k3s + Agones for studios that want an authoritative game-server fleet.

Everything below ships in-tree today. New to the model? Read [`docs/CONCEPTS.md`](docs/CONCEPTS.md) for tenants, projects, and API keys, or [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the request path.

## Quickstart

```bash
git clone --recurse-submodules https://github.com/automoto/gg-scale.git
cd gg-scale
make up
curl -s localhost:8080/v1/healthz
```

Expected: `{"status":"ok"}` with header `X-API-Version: v1`.

`make up` starts the **basic stack**: `ggscale-server`, Postgres, and MailHog (SMTP catcher with a web UI at `http://localhost:8025`).

If you cloned without submodules, run `git submodule update --init --recursive` before using the k3s + Agones profile.

### Bootstrap the dashboard

1. Read the one-time token: `cat ./data/bootstrap.token` (also printed in `docker compose logs ggscale-server` at first startup).
2. Open `http://localhost:8080/v1/dashboard/setup`, create the first platform admin, then sign in at `/v1/dashboard/login`.
3. Create a **tenant**, a **project**, and a **secret API key** (shown once; store it safely). Every player-facing `/v1/*` call authenticates with `Authorization: Bearer <api_key>`.

## Player and session features

Everything durable is backed by Postgres with row-level security per tenant. These endpoints require an end-user session (`X-Session-Token`), which a player obtains through one of the auth flows; the Go SDK wraps them, and the raw routes live under `/v1/`.

| Feature | Endpoints | What it does |
|---|---|---|
| **Accounts & auth** | `/v1/auth/*` | Email/password (bcrypt), anonymous, and `custom-token` sign-in, the last federating an existing identity provider through a tenant-signed JWT. Sessions are 15-minute HS256 access tokens paired with rotating 30-day refresh tokens, stored only as SHA-256 hashes. Email verification uses attempt-capped codes, and signup is anti-enumeration (uniform `202`). |
| **Profiles** | `/v1/profile` | Per-player display name, avatar, and arbitrary JSON metadata; read and partial-update (`PATCH`). |
| **Friends** | `/v1/friends/*` | Directed friend edges with a request → accept/reject/block state machine. Requests are idempotent and illegal transitions return `409`; the friends list is enriched with each friend's identity and live presence. |
| **Presence** | `/v1/presence` | Online state plus a free-form status (≤32 chars), upserted per player. Every update fans out over WebSocket to the player's accepted friends. |
| **Game sessions** | `/v1/game-session/*` | The room players share before and during a match: create or join by 6-character code, heartbeat to stay a member, and leave. Joins are row-locked to respect capacity (up to 64), stale peers are pruned on a 30-second window, and the session ends when the host leaves (4-hour TTL). Each peer row carries the player's public UDP endpoint for P2P connect. |
| **Invites** | `/v1/invite/*` | Short-lived (5-minute) invites from a session host or member to an accepted friend; create, list pending, and cancel or decline. Pushed over WebSocket when the recipient is online. |
| **Storage** | `/v1/storage/objects/*` | Per-player JSON object store for saves and settings, up to 1 MiB per value. Optimistic concurrency via a monotonic `version` and `If-Match` (`412` on conflict); cursor-paginated listing with key-prefix filters. |
| **Leaderboards** | `/v1/leaderboards/*` | Append-only score entries ranked with SQL window functions on ascending or descending boards. `top` and `around-me` (0-based ranks) are open reads; submission is server-authoritative, requiring a secret key and the `leaderboard:submit` permission. |
| **Realtime** | `/v1/ws` | A per-player WebSocket (one connection per end-user, newer-wins) the server uses to push presence changes, invites, and match-ready events. Concurrency is capped per tenant and per end-user by billing tier, with 30-second server heartbeats. |
| **Relay** | `/v1/relay/credentials` | An embedded TURN server (pion/turn) for NAT traversal in P2P games. Issues short-lived (5-minute) TURN-REST credentials (HMAC-SHA1 over an expiry-scoped username); UDP listener on `:3478`. |

Friends, presence, game sessions, and invites are the social layer most multiplayer games rebuild from scratch; ggscale ships them so you don't have to.

## Matchmaking and the server browser

Two ways to put players into a game, depending on whether your title runs dedicated servers or peer-to-peer:

- **Matchmaker** (`/v1/matchmaker/tickets`). A client posts a ticket and polls or cancels it. Workers match tickets within a `(fleet, region, game-mode)` bucket, allocate a host from the configured fleet backend, and push a `match_ready` message carrying the connection address over WebSocket. The queue runs on Postgres `LISTEN`/`NOTIFY` with a two-phase claim, so a crashed worker's tickets are reclaimed rather than dropped, and a server is deallocated if no client turns up to take it.
- **Server browser** (`/v1/fleets/{fleet}/servers`). For persistent dedicated servers: each server heartbeats its address and current player count into an in-memory registry, and any authenticated player lists the live servers for a fleet and connects directly.

## Fleet backends

A *fleet* is the pool of game-server instances ggscale allocates from when a match needs a host. The backend is chosen with `FLEET_BACKEND`; allocations always return a `{host, port}` clients connect to. Per-fleet templates (image, port, health probe) are managed in the dashboard, not in env vars.

| Backend | `FLEET_BACKEND` | Best for |
|---|---|---|
| **Docker** (default) | `docker` | Single-VPS self-hosting. `ggscale-server` talks to the local Docker daemon and spawns a game-server container per allocation. Set `GAME_SERVER_PUBLIC_IP` to the address clients can reach. |
| **Agones** | `agones` | Studios running on Kubernetes. Allocations come from an Agones `Fleet`, which handles autoscaling, region selectors, and rolling updates. Dev uses a single-node k3s cluster. |
| **Plugin** | `plugin:<name>` | Anything else. `ggscale-server` runs an out-of-tree `ggscale-fleet-<name>` binary from `FLEET_PLUGIN_DIR`, so you can provision against your own cloud or VM provider without forking. See [`docs/fleet-plugins.md`](docs/fleet-plugins.md). |

The matchmaker and the allocate endpoint return a not-implemented error until a backend is wired in, so the basic stack runs fine without one.

## Docker Compose setups

Four scenarios, four files. Each is standalone; pick the file that matches what you're doing.

| Scenario | File | Make target | What's in it |
|---|---|---|---|
| **Basic dev** | `docker-compose.yml` | `make up` | `ggscale-server` + Postgres + MailHog. Quick local runs and the self-hosting starter. |
| **Fleet, Docker backend** | `compose/fleet-docker.yml` | `make up-fleet-docker` | Basic dev plus `FLEET_BACKEND=docker` and a `/var/run/docker.sock` mount. Allocations spawn game-server containers on demand. |
| **Fleet, k3s + Agones** | `compose/fleet-agones.yml` | `make up-fleet-agones` + `make agones-install` | Basic dev plus a single-node k3s cluster and the Agones controller. Allocations come from an Agones Fleet. |
| **Full dev stack** | `compose/full.yml` | `make up-full` | Fleet/Docker backend plus Prometheus (`:9090`). The contributor environment. |

Scenario files `include:` the basic compose, so service definitions live in one place. Run compose from the repo root:

```bash
docker compose -f compose/fleet-docker.yml up -d --wait
```

### Picking GAME_SERVER_PUBLIC_IP

For the Docker fleet backend, set `GAME_SERVER_PUBLIC_IP` in `.env` to the host IP your clients can reach. Allocations return `{host, port}` and clients connect directly to that address. See [`docs/SELF_HOSTING.md`](docs/SELF_HOSTING.md) for production setup and UDP security.

### k3s + Agones on macOS

Run Colima first; Docker Desktop's host networking breaks Agones UDP reachability.

```bash
brew install colima
colima start --network-address --cpus 4 --memory 8
make up-fleet-agones && make agones-install
```

Linux contributors need nothing extra. The Agones profile mounts manifests from the `infra/k8s/` checkout by default. To use a separate checkout of `gg-scale-infra`, run make with `GGSCALE_INFRA_DIR=/path/to/gg-scale-infra`.

## Go SDK

The client module is published as **`github.com/automoto/ggscale-go`** (sibling repo `ggscale-go`). It covers auth, storage, leaderboards, and profiles behind a pluggable `Transport`, and is the easiest way to call the API from a Go game server.

## Common commands

| Target | What it does |
|---|---|
| `make up` / `make down` / `make clean` | Basic dev stack (server + Postgres + SMTP). |
| `make up-fleet-docker` / `make down-fleet-docker` | Fleet feature with the Docker backend. |
| `make up-fleet-agones` / `make down-fleet-agones` | Fleet feature with k3s + Agones. Follow with `make agones-install`. |
| `make up-full` / `make down-full` / `make clean-full` | Full dev stack (Prometheus + Docker fleet). |
| `make test` | Unit tests with `-race`. |
| `make test-integration` | Integration tests (`-tags=integration`, Postgres via testcontainers). |
| `make e2e` | Live compose checks (`-tags=e2e`); run after the relevant `up-*`. |
| `make lint` | `golangci-lint`. |

## License

Apache 2.0. See [LICENSE](LICENSE).
