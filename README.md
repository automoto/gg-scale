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
curl localhost:8080/v1/healthz
```

Expected: `{"status":"ok"}` with header `X-API-Version: v1`.

This starts the simple stack: the ggscale server, Postgres, and a local SMTP server (MailHog). MailHog's web UI is available at `http://localhost:8025`.

## Docker Compose setups

There are two compose configurations:

**Simple stack** (`docker-compose.yml`) — for self-hosting and quick local runs:
- `ggscale-server`, `postgres`, `mailhog` (SMTP)
- `make up` / `make down`

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
| `make up-dev` | Start the full dev stack. |
| `make down-dev` | Tear the full dev stack down. |
| `make up-k8s` | Start k3s + Agones (full stack, macOS: run Colima first). |
| `make test` | Unit tests with `-race`. |
| `make lint` | `golangci-lint`. |

## License

Apache 2.0. See [LICENSE](LICENSE).
