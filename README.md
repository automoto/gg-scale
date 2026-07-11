# ggscale

Open-source, self-hostable backend for multiplayer games. One Go binary and a Postgres database give you player accounts, saves, leaderboards, social features, matchmaking, and a game-server fleet. Run it on a single VPS, keep your data, and keep the game online as long as you want.

## Local Development & Quickstart

```bash
git clone --recurse-submodules https://github.com/automoto/gg-scale.git
cd gg-scale
make up
curl -s localhost:8080/v1/healthz
```

Expected: `{"status":"ok"}` with header `X-API-Version: v1`.

`make up` starts the **basic stack**: `ggscale-server`, Postgres, and MailHog (SMTP catcher with a web UI at `http://localhost:8025`).

## Onboarding (Control Panel Setup)

1. Read the one-time token: `cat ./data/bootstrap.token` (also printed in `docker compose logs ggscale-server` at first startup).
2. Open `http://localhost:3001/v1/control-panel/setup`, create the first platform admin, then sign in.
3. Create a **tenant**, a **project**, and a **secret API key**. Every player-facing `/v1/*` call authenticates with `Authorization: Bearer <api_key>`.

For detailed onboarding, please see the [Wiki Onboarding Guide](https://github.com/automoto/gg-scale/wiki).

## Common Commands

Run `make help` for the full list.

| Target | What it does |
|---|---|
| `make up` / `make down` / `make clean` | Basic dev stack (server + Postgres + SMTP). |
| `make up-fleet-docker` / `make down-fleet-docker` | Fleet feature with the Docker backend. |
| `make test` | Unit tests with `-race`. |
| `make check` | Lint + unit tests, same gate as CI. |

## Documentation

Full documentation, including Architecture, Features, API Route descriptions, and Onboarding guides, has been moved to our GitHub Wiki:

👉 **[ggscale GitHub Wiki Placeholder](https://github.com/automoto/gg-scale/wiki)**
