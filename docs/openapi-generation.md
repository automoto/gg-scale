# OpenAPI spec generation

`openapi.yaml` at the repo root describes the `/v1` JSON API, and the SDKs are
generated from it. Do not edit it by hand. Regenerate it after adding or
changing `/v1` routes or handler types:

```sh
make openapi
```

Every `/v1` route is a typed [Huma v2](https://huma.rocks) `huma.Register`
call with its request input, response output and security metadata, so the
spec comes from the handlers themselves and stays in step with the wire.
`make openapi` runs `go run ./cmd/openapi-dump openapi.yaml`, which builds the
operations in process and writes `api.OpenAPI().YAML()`. It needs no external
analyzer or live dependencies.

The HTML surfaces (control panel, player pages, web assets) are not Huma
operations and are left out of the spec.

## How it works

- `internal/httpapi/openapi.go`: `OpenAPIDoc(version)` registers every `/v1`
  operation into one shared `huma.OpenAPI` document. The handler closures are
  registered but never called, so a zero `Deps` is enough. Its `register*`
  list mirrors the registrations in `NewRouter`, which spreads them across
  middleware-scoped chi groups; the document needs only the operation
  metadata, so here they share one adapter.
- `cmd/openapi-dump`: a thin `main` that calls `OpenAPIDoc` and writes the
  YAML. Its `specVersion` constant sets `info.version`.
- Schemas, request and response bodies, status codes, and the
  `PublishableKey`, `SecretKey` and `PlayerSession` security schemes all come
  from each operation's Go types and `huma.Operation` metadata.
- An operation that refuses a publishable key lists only `SecretKey`
  (`secretKeySecurity` in `humaapi.go`). Every other operation lists
  `PublishableKey`, whose description says a secret key also works. Score
  submit lists both, because the leaderboard decides. The integration test
  `TestAPIKeyType_spec_matches_router` sends a publishable key to every
  operation and checks the `403`s against the spec.

## Guard tests

`internal/httpapi/openapi_test.go` checks the generator against the router and
the committed file:

- `TestOpenAPIDoc_covers_expected_paths` fails when the `register*` list in
  `OpenAPIDoc` drifts from the routes the server serves.
- `TestOpenAPIDoc_committed_spec_is_current` regenerates the document and
  diffs it against the committed `openapi.yaml`. It catches a route or schema
  field added without running `make openapi`, which the path test cannot,
  since that test checks only the in-memory document. Keep its `specVersion`
  equal to the one in `cmd/openapi-dump`.
- `TestOpenAPIDoc_never_documents_internal_surface` keeps control panel and
  other non-`/v1` routes out of the spec.
- `TestOpenAPIDoc_schemas_carry_examples` requires example values on wire
  structs. Redoc builds its response samples from them, and without examples
  the rendered docs show empty or null samples.

## Hand-maintained additions

Huma cannot fully describe two routes, so `OpenAPIDoc` patches them into the
document after registration. Both patches live in Go next to the types they
reference.

- `POST /v1/server/player-sessions/verify` is a Huma body callback that owns
  the raw request and response to keep its opaque `401`, so Huma emits no
  schema for it. `enrichVerifyOp` adds the request body, the `200` response
  and the opaque `401` from the `playerVerifyRequest` and
  `playerVerifyResponse` types. The handler is in
  `internal/httpapi/player_sessions_verify.go`.
- `GET /v1/ws`, the realtime WebSocket route, is a plain chi handler, so
  `addWebSocketStub` adds a stub operation. OpenAPI cannot describe the socket
  protocol itself.

## Notes for SDK generators

- Documented paths are canonical, with no trailing slash: `/v1/friends`,
  `/v1/profile`, `/v1/game-session`. The server also accepts the
  trailing-slash form, but the spec lists only the canonical path.
- Error responses are `application/problem+json` (`{title, status, detail,
  errors[]}`), except the opaque `401` from verify.
