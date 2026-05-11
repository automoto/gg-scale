# ggscale

Open-source backend for multiplayer games. We want to do for game hosting what WordPress did for websites: make it cheap, self-hostable, and survivable past the studio that built it.

**ggscale is in active development and isn't ready for production yet, we're currently doing alpha testing and making bug fixes**

## What it is

ggscale is a single Go binary. Drop it on any Linux box, point it at a Postgres URL, and you have auth, storage, leaderboards, lobbies, matchmaking, and a P2P relay. A second compose file adds k3s + Agones for studios that want an authoritative game-server fleet.

## Quickstart

```bash
git clone https://github.com/automoto/gg-scale.git
cd gg-scale
make up
curl -s localhost:8080/v1/healthz
```

Expected: `{"status":"ok"}` with header `X-API-Version: v1`.

This starts the **simple stack**: ggscale-server, Postgres, and MailHog (SMTP + web UI at `http://localhost:8025`).

### Multi-tenant bootstrap (dashboard)

1. Read the one-time token: `cat ./data/bootstrap.token` (or check `docker compose logs ggscale-server` once at startup).
2. Open `http://localhost:3001/v1/dashboard/setup`, create the first platform admin, then sign in at `/v1/dashboard/login`.
3. Create a **tenant**, **project**, and **secret API key** (shown once; store it safely). All player-facing `/v1/*` calls use `Authorization: Bearer <api_key>`.

### Go SDK (v0.1)

The client module is published as **`github.com/automoto/ggscale-go`** (sibling repo `ggscale-go`). It covers auth, storage, leaderboards, and profile with a pluggable `Transport`.

New to the model? Read [`docs/CONCEPTS.md`](docs/CONCEPTS.md) for tenants, projects, and API keys.

## Docker Compose setups

There are two compose configurations:

**Simple stack** (`docker-compose.yml`) — for self-hosting and quick local runs:
- `ggscale-server`, `postgres`, `mailhog` (SMTP)
- `make up` / `make down`

**Game server stack** (`ops/docker-compose.gameserver.yml`) — simple self-hosting with a dedicated game server alongside ggscale, no Kubernetes required:
- Everything in the simple stack plus a `doomerang-server` container (UDP port 7654)
- `make up-gameserver` / `make down-gameserver`
- Set `GAME_SERVER_PUBLIC_IP` to your host's public IP so clients know where to connect. See [`docs/SELF_HOSTING.md`](docs/SELF_HOSTING.md) for production setup, UDP security, and the path to k3s + Agones when you outgrow static containers.

**Full dev stack** (`ops/full-stack-docker-compose.yml`) — for contributors who need the complete environment:
- Everything in the simple stack plus Prometheus, Stripe mock, dashboard stub, and optional k3s + Agones
- `make up-dev` / `make down-dev`

### k8s profile (contributors only)

The k8s profile requires Colima on macOS — Docker Desktop's host networking breaks Agones UDP reachability.

```bash
brew install colima
colima start --network-address --cpus 4 --memory 8
make up-k8s && make agones-install
```

Linux contributors need nothing extra.

## Common commands

| Target | What it does |
|---|---|
| `make up` | Start the simple stack (server + postgres + smtp). |
| `make down` | Tear the simple stack down. |
| `make clean` | Tear down + delete volumes. |
| `make up-gameserver` | Start ggscale + a dedicated game server container (no k8s). |
| `make down-gameserver` | Tear the game server stack down. |
| `make up-dev` | Start the full dev stack. |
| `make down-dev` | Tear the full dev stack down. |
| `make up-k8s` | Start k3s + Agones (full stack, macOS: run Colima first). |
| `make test` | Unit tests with `-race`. |
| `make test-integration` | Integration tests (`-tags=integration`, Postgres via testcontainers). |
| `make e2e` | Live compose checks (`-tags=e2e`); run after `make up`. |
| `make lint` | `golangci-lint`. |

## License

Apache 2.0. See [LICENSE](LICENSE).
