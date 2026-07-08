# OpenAPI spec generation

`openapi.yaml` (repo root) describes the `/v1` JSON API — the surface SDKs are
generated from. It is produced from the router/handler source, not written by
hand:

```sh
make openapi        # regenerate after adding/changing /v1 routes or handler types
```

The spec is emitted directly from the [Huma v2](https://huma.rocks) operations
that back every `/v1` route. Each endpoint is a typed `huma.Register` call
(request input, response output, security metadata), so the document is
generated from the handlers themselves and **cannot drift** from the wire.
`make openapi` runs `go run ./cmd/openapi-dump openapi.yaml`, which builds the
operation set in-process and writes `api.OpenAPI().YAML()`. It is fast and
side-effect-free — no external analyzer, no live dependencies, no OOM risk.

The HTML surfaces (control panel, player pages, web assets) are not huma
operations and are intentionally absent — only the JSON API is specified.

## How it works

- `internal/httpapi/openapi.go` — `OpenAPIDoc(version)` registers every `/v1`
  operation into one shared `huma.OpenAPI` document. It needs no live
  dependencies: the handler closures are registered but never invoked, so a
  zero `Deps` suffices. The `register*` list there mirrors `NewRouter`'s
  registrations (NewRouter spreads them across middleware-scoped chi groups;
  the doc only needs the operation metadata, so they collapse onto one
  adapter). `TestOpenAPIDoc_covers_expected_paths` fails if that list drifts.
- `cmd/openapi-dump` — thin `main` that calls `OpenAPIDoc` and writes the YAML.
- Schemas, request/response bodies, status codes, and `ApiKeyAuth` /
  `PlayerSession` security all come straight from each operation's Go types and
  `huma.Operation` metadata. No conventions to keep extraction happy — the
  types *are* the spec.

## Hand-maintained additions

Two routes can't be fully described by huma on their own; `OpenAPIDoc` patches
them into the document after registration:

- **`POST /v1/server/player-sessions/verify`** — its handler is a huma
  body-callback (it owns the raw request/response to keep the opaque-401 wire),
  so huma emits no schema for it. `enrichVerifyOp` fills in the request body,
  the 200 response, and the opaque 401 from the `playerVerifyRequest` /
  `playerVerifyResponse` types. Handler:
  `internal/httpapi/player_sessions_verify.go`.
- **`GET /v1/ws`** — the realtime WebSocket route stays a plain chi handler
  (not a huma op), so `addWebSocketStub` hand-adds a stub operation. OpenAPI
  cannot describe the socket protocol.

Both live in Go alongside the types they reference — there is no separate
overlay file or merge tool.

## Notes for SDK generators

- Documented paths are canonical (no trailing slash), e.g. `/v1/friends`,
  `/v1/profile`, `/v1/game-session`. The server also answers the
  trailing-slash form at runtime, but the spec lists only the canonical path.
- Error responses are `application/problem+json` (`{title, status, detail,
  errors[]}`) except the deliberately-opaque verify 401.
