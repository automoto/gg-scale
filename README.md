# GG Scale
[![DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/automoto/gg-scale)

GG Scale is an open source platform for multiplayer games. It provides the features you need to build an online game:

1. Authentication and social - player accounts, friends, game sessions, lobbies, matchmaking, parties and chat (coming soon)
2. Networking - peer-to-peer relays, robust APIs, real-time connections using WebSockets
3. Data storage - game project settings, cloud saves, leaderboards, object storage (coming soon)
4. Management - a control panel, API and MCP for managing your projects and the platform itself
5. Dedicated servers (beta) - a flexible fleet backend that allows you to provision and manage virtual machines or containers using plugins for supported backend providers like Kubernetes, OpenStack and AWS

<img src="docs/dashboard.png" alt="ggscale control panel" width="600">

## GG Scale Cloud

If you prefer a managed service, run your game on GG Scale Cloud and leave the hosting to us.

**[Try GG Scale Cloud →](https://ggscale.com)**

## Documentation

Full documentation is at [docs.ggscale.com](https://docs.ggscale.com/wiki/Home/): architecture, features, API routes, and onboarding guides.

The [HTTP API reference](https://docs.ggscale.com/api/) is generated from our OpenAPI Specification [`openapi.yaml`](openapi.yaml).

Connect your coding agent to the public docs MCP server so it can read the guides and the API reference:

```sh
claude mcp add --transport http ggscale-docs https://docs.ggscale.com/mcp
```

## SDKs

GG Scale provides a robust API that you can use from your preferred programming language or game engine. We also provide SDKs for convenience:

- Go: [ggscale-go](https://github.com/automoto/ggscale-go)
- C# (Unity, MonoGame, Godot): [gg-scale-sdk-C-Sharp](https://github.com/automoto/gg-scale-sdk-C-Sharp)

## Local development and quickstart

You need Docker and make. The tests also need Go 1.26.10 or later, the version in `go.mod`.

```bash
git clone https://github.com/automoto/gg-scale.git
cd gg-scale
cp .env.example .env
make up
curl -s localhost:8080/v1/healthz
```

The health check returns `"status": "ok"` with the server version and commit, and the header `X-API-Version: v1`.

`make up` starts the basic stack: `ggscale-server`, Postgres, and Mailpit, an SMTP catcher with a web UI at `http://localhost:8025`. If the Mailpit ports are already in use, set `MAILPIT_SMTP_PORT` and `MAILPIT_HTTP_PORT`.

## Control panel setup

1. Read the one-time token with `make bootstrap-token`. The server logs show the token file path (`./data/bootstrap.token`) but never the token value.
2. Open `http://localhost:8080/v1/control-panel/setup`, create the first platform admin, and sign in.
3. Create an Account Tenant, a Game Project, and a publishable API key. Your game client sends the publishable key on every call as `Authorization: Bearer <api_key>`. Create a secret key only for a game server or backend, and never ship it in a game.

The [onboarding guide](https://docs.ggscale.com/wiki/onboarding/) has more information.

## Common commands

Run `make help` for the full list.

| Target | What it does |
|---|---|
| `make up` / `make down` / `make clean` | Basic dev stack (server, Postgres, SMTP). |
| `make bootstrap-token` | Print the control panel bootstrap token from `./data/bootstrap.token`. |
| `make test` | Unit tests with `-race`. |
| `make coverage` | Create `coverage/coverage.out` and `coverage/coverage.html`. |
| `make test-integration` | Integration tests (Postgres via Testcontainers). |
| `make test-e2e` | End-to-end suite against the running `make up` stack. |
| `make check` | Lint and unit tests, the same gate as CI. |

## Contributing and license

See [CONTRIBUTING.md](CONTRIBUTING.md). GG Scale is licensed under the [GNU GPL v3](LICENSE). See [NOTICE](NOTICE).
