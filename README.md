# ggscale

Multiplayer game backend-as-a-service. Apache 2.0 OSS core; managed SaaS at
[ggscale.io](https://ggscale.io) (TBD). Authoritative game-server fleet on
K3s + Agones, multi-tenant control plane in Go, single-binary self-host.

> Status: pre-v1.0. See [docs/mvp.md](docs/mvp.md) and [docs/ROADMAP.md](docs/ROADMAP.md).

## Built for the full game lifecycle

ggscale is the open-source backend for the entire lifecycle of a multiplayer
game — from launch day to graceful sunset. The same OSS core, Agones fleet,
and STUN/TURN relay underwrite three audiences:

| Audience | What ggscale offers | Ships in |
|---|---|---|
| **Indie / mid-tier studios** building new live-service games | Hybrid networking (authoritative servers + STUN/TURN relay), 3-region OVH managed cloud behind Cloudflare LB, or self-host on a small commodity VM. Cheapest credible BaaS in the market. | v1.0 |
| **Publishers shutting down** a live-service title | `ggscale Sunset` — B2B engagement that ports the title to ggscale-OSS for community self-hosting, or ships an offline-mode patch when porting is infeasible. Satisfies CA AB 2426 / Protect Our Games Act. | v1.8+ |
| **Player communities** of sunset titles | `ggscale Community Hosting` — branded white-label hosting of private dedicated server instances, with royalty back to the publisher. | v1.8+ |

A title shipped on ggscale at launch can mark its store page
"preservation-ready" and skip the AB 2426 "revocable license" disclaimer —
because it can already run independently of ggscale's infrastructure via
the OSS self-host path. See [docs/LIFECYCLE.md](docs/LIFECYCLE.md) for the
full strategy.

## Quickstart

```bash
git clone https://github.com/ggscale/ggscale.git
cd ggscale
cp .env.example .env
make up
curl localhost:8080/v1/healthz
```

Expected: `{"status":"ok"}` with header `X-API-Version: v1`.

### macOS contributors

Docker Desktop's host networking is unreliable on darwin and breaks Agones
UDP `hostPort` reachability. The K8s profile (`make up-k8s`) **requires
Colima** — `make up-k8s` runs `preflight-k8s` first, which fails fast with
install instructions if Colima isn't installed or running.

```bash
brew install colima
colima start --network-address --cpus 4 --memory 8
make up-k8s
make agones-install
make e2e
```

The lite stack (`make up`) doesn't need Colima — it works on Docker Desktop.

Linux contributors: nothing extra to install. To bypass the macOS Colima
check (e.g. when running k3s in a Linux VM you manage separately), set
`GGSCALE_SKIP_COLIMA_CHECK=1`.

## Development

| Target | What it does |
|---|---|
| `make up` | Bring the lite compose stack up (no k8s). |
| `make up-k8s` | Add k3s + Agones (macOS: run Colima first). |
| `make agones-install` | Apply the pinned Agones manifest. |
| `make migrate` | Apply pending DB migrations. |
| `make migrate-new NAME=foo` | Scaffold a new migration pair. |
| `make test` | Unit tests with `-race`. |
| `make test-integration` | Tests tagged `integration` (requires Docker for testcontainers). |
| `make e2e` | End-to-end suite against the live stack (requires `make up-k8s` + `make agones-install` first). |
| `make lint` | `golangci-lint`. |
| `make vulncheck` | `govulncheck`. |
| `make logs` | Tail `ggscale-server` logs. |
| `make psql` | Open a `psql` shell against the dev DB. |
| `make down` | Tear the compose stack down. |
| `make clean` | Tear down + delete volumes + remove `.k3s/` kubeconfig. |

TDD is the rule. Tests fail before implementation; see
[CONTRIBUTING.md](CONTRIBUTING.md).

## Architecture

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the compose stack,
[docs/RUNBOOK.md](docs/RUNBOOK.md) for failure-mode recovery,
[docs/HA.md](docs/HA.md) for the high-availability runbook (current HA
posture, rolling deployments, Postgres replication + cache HA),
[docs/mvp.md](docs/mvp.md) for the engineering plan,
[docs/LIFECYCLE.md](docs/LIFECYCLE.md) for the canonical product strategy
(three audiences, regulatory tailwind, v1.0 vs. v1.8+ split), and
[docs/ROADMAP.md](docs/ROADMAP.md) for the public versioned roadmap.

## License

Apache 2.0. See [LICENSE](LICENSE).
