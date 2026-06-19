# ggscale

Open-source backend for multiplayer games. We want to do for game hosting what WordPress did for websites: make it cheap, self-hostable, and survivable past the studio that built it.

**ggscale is in active development and isn't ready for production yet, we're currently doing alpha testing and making bug fixes**

## What it is

ggscale is a single Go binary. Drop it on any Linux box, point it at a Postgres URL, and you have auth, storage, leaderboards, lobbies, matchmaking, and a P2P relay. A second compose file adds k3s + Agones for studios that want an authoritative game-server fleet.

## Quickstart

```bash
git clone --recurse-submodules https://github.com/automoto/gg-scale.git
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

If you cloned without submodules, run `git submodule update --init --recursive`
before using the k3s + Agones profile.

## Docker Compose setups

Four scenarios, four files. Each one is standalone — pick the file that matches what you're doing.

| Scenario | File | Make target | What's in it |
|---|---|---|---|
| **Basic dev** | `docker-compose.yml` | `make up` | ggscale-server + postgres + mailhog. Quick local runs, self-hosting starter. |
| **Fleet, Docker backend** | `compose/fleet-docker.yml` | `make up-fleet-docker` | Basic dev + `FLEET_BACKEND=docker` + `/var/run/docker.sock` mount. `POST /v1/fleet/allocate` spawns game-server containers on demand. |
| **Fleet, k3s + Agones** | `compose/fleet-agones.yml` | `make up-fleet-agones` + `make agones-install` | Basic dev + a single-node k3s cluster + Agones controller. Allocations come from an Agones Fleet. |
| **Full dev stack** | `compose/full.yml` | `make up-full` | Fleet/Docker backend + Prometheus (`:9090`) + Stripe mock. Contributor environment. |

Scenario files `include:` the basic dev compose, so service definitions live in one place. Run compose from the repo root:

```bash
docker compose -f compose/fleet-docker.yml up -d --wait
```

### Picking GAME_SERVER_PUBLIC_IP

For the Docker fleet backend, set `GAME_SERVER_PUBLIC_IP` in `.env` to the host IP your clients can reach. Allocations return `{host, port}`; clients connect directly to that address. See [`docs/SELF_HOSTING.md`](docs/SELF_HOSTING.md) for production setup and UDP security.

### k3s + Agones on macOS

Run Colima first — Docker Desktop's host networking breaks Agones UDP reachability.

```bash
brew install colima
colima start --network-address --cpus 4 --memory 8
make up-fleet-agones && make agones-install
```

Linux contributors need nothing extra.

The Agones profile mounts manifests from the `infra/k8s/` checkout by default.
To use a separate checkout of `gg-scale-infra`, run make with
`GGSCALE_INFRA_DIR=/path/to/gg-scale-infra`.

## Common commands

| Target | What it does |
|---|---|
| `make up` / `make down` / `make clean` | Basic dev stack (server + postgres + smtp). |
| `make up-fleet-docker` / `make down-fleet-docker` | Fleet feature with the Docker backend. |
| `make up-fleet-agones` / `make down-fleet-agones` | Fleet feature with k3s + Agones. Follow with `make agones-install`. |
| `make up-full` / `make down-full` / `make clean-full` | Full dev stack (prometheus + docker fleet + stripe-mock). |
| `make test` | Unit tests with `-race`. |
| `make test-integration` | Integration tests (`-tags=integration`, Postgres via testcontainers). |
| `make e2e` | Live compose checks (`-tags=e2e`); run after the relevant `up-*`. |
| `make lint` | `golangci-lint`. |

## License

Apache 2.0. See [LICENSE](LICENSE).
