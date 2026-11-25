# ggscale

ggscale is an open source backend for multiplayer games. It has player accounts and auth, game sessions and lobbies, matchmaking and parties, peer-to-peer relay, dedicated servers (beta), cloud saves, leaderboards, friends, and a control panel to manage it all.

<img src="docs/dashboard.png" alt="ggscale control panel" width="600">

## ggscale Cloud

If you prefer a managed service, run your game on ggscale Cloud and leave the hosting to us.

**[Try ggscale Cloud →](https://ggscale.com)**

## Documentation

Full documentation is at [docs.ggscale.com](https://docs.ggscale.com/wiki/Home/): architecture, features, API routes, and onboarding guides.

The [HTTP API reference](https://docs.ggscale.com/api/) is generated from [`openapi.yaml`](openapi.yaml).

To let a coding agent read the guides and the API reference, connect it to the public docs server. It needs no token.

```sh
claude mcp add --transport http ggscale-docs https://docs.ggscale.com/mcp
```

Operators running relay nodes or rolling deploys should read [Relay Operations](https://docs.ggscale.com/wiki/Relay-Operations/), [Process Lifecycle](https://docs.ggscale.com/wiki/Process-Lifecycle/), and [Operator Tokens](https://docs.ggscale.com/wiki/Operator-Tokens/) in the docs. Developers changing `/v1` routes should read [OpenAPI spec generation](docs/openapi-generation.md).

For more technical detail, ask DeepWiki:

[![DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/automoto/gg-scale)

## SDKs

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

The [onboarding guide](https://docs.ggscale.com/wiki/onboarding/) covers the rest.

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

See [CONTRIBUTING.md](CONTRIBUTING.md). ggscale is licensed under the [GNU GPL v3](LICENSE).

Copyright (C) 2026 Build W Solutions Foundation. See [NOTICE](NOTICE).
