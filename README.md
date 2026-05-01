# ggscale

Open-source backend for multiplayer games. We want to do for game hosting what WordPress did for websites: make it cheap, self-hostable, and survivable past the studio that built it.

**ggscale is in active development and isn't ready for production yet, we're currently doing alpha testing and making bug fixes**

## What it is

ggscale is a single Go binary. Drop it on any Linux box, point it at a Postgres URL, and you have auth, storage, leaderboards, lobbies, matchmaking, and a P2P relay. A second compose file adds k3s + Agones for studios that want an authoritative game-server fleet.

## Quickstart

```bash
git clone https://github.com/automoto/gg-scale.git
cd gg-scale
cp .env.example .env
make up
curl localhost:8080/v1/healthz
```

Expected: `{"status":"ok"}` with header `X-API-Version: v1`.

### macOS contributors

The K8s profile requires Colima — Docker Desktop's host networking breaks Agones UDP reachability.

```bash
brew install colima
colima start --network-address --cpus 4 --memory 8
make up-k8s && make agones-install
```

The lite stack (`make up`) works on Docker Desktop. Linux contributors need nothing extra.

## Common commands

| Target | What it does |
|---|---|
| `make up` | Start the lite stack (no k8s). |
| `make up-k8s` | Start with k3s + Agones (macOS: run Colima first). |
| `make test` | Unit tests with `-race`. |
| `make lint` | `golangci-lint`. |
| `make down` | Tear the stack down. |
| `make clean` | Tear down + delete volumes. |

## License

Apache 2.0. See [LICENSE](LICENSE).
