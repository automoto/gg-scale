# gg-scale

The open platform for multiplayer games. A simple web service and a Postgres database give you everything you need to build and run an online game with players.

## Documentation

Full documentation, including Architecture, Features, API Route descriptions, and Onboarding guides, has been moved to our GitHub Wiki:

**[gg-scale wiki](https://github.com/automoto/gg-scale/wiki)**

DeekWiki documentation is available here as well for more technical details:

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/automoto/gg-scale)

## Local Development & Quickstart

```bash
git clone https://github.com/automoto/gg-scale.git
cd gg-scale
make up
curl -s localhost:8080/v1/healthz
```

Expected: `{"status":"ok"}` with header `X-API-Version: v1`.

`make up` starts the basic stack: `ggscale-server`, Postgres, and Mailpit (SMTP catcher with a web UI at `http://localhost:8025`). The published SMTP and HTTP ports can be overridden with `MAILPIT_SMTP_PORT` and `MAILPIT_HTTP_PORT` if they are already occupied.

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
| `make test` | Unit tests with `-race`. |
| `make test-integration` | Integration tests (Postgres via testcontainers). |
| `make e2e` | End-to-end suite against the running `make up` stack. |
| `make check` | Lint + unit tests, same gate as CI. |

## Testing

This repo is self-contained: every test target above runs with only Docker
and this checkout, and covers the GA feature set — auth and players, game
sessions and signaling, matchmaking, P2P relay, saves/storage, leaderboards,
friends and invites, and the control panel.

The game-server **fleet** feature is beta and not yet ready for production.
