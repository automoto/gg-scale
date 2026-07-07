# OpenAPI spec generation

`openapi.yaml` (repo root) describes the `/v1` JSON API — the surface SDKs are
generated from. It is produced from the router/handler source, not written by
hand:

```sh
make openapi        # regenerate after adding/changing /v1 routes or handler types
```

The pipeline is [ehabterra/apispec](https://github.com/ehabterra/apispec)
driven by `apispec.yaml` (repo root) via `scripts/gen-openapi.sh`. The HTML
surfaces (dashboard, player pages, web assets) are excluded — only the JSON
API is specified.

## How handlers map into the spec

apispec statically traces chi route registrations into handler bodies:

- `decodeJSON(w, r, &req)` → request body schema from `req`'s type
- `writeJSON(w, body)` → 200 response schema from `body`'s type
- `writeJSONStatus(w, status, body)` → response schema under `status`
- `http.Error` / `w.WriteHeader` → error/no-body status codes
- `tenant.New` middleware → `ApiKeyAuth` (bearer) security requirement
- `playerauth.New` middleware → `PlayerSession` (X-Session-Token) requirement

Conventions that keep extraction accurate:

- Respond through `writeJSON` / `writeJSONStatus`, not ad-hoc
  `WriteHeader`+`Encode` pairs.
- Handler factories must return a closure directly (`return func(w, r) {...}`).
  Returning another factory's result (`return otherFactory(...)`) breaks the
  tracer's route→handler linkage — delegate to a plain function from inside
  the closure instead (see `friendAcceptHandler` / `changeFriendStatus`).
- The tracer resolves a response body's type through exactly one level of
  indirection (handler call site → helper parameter). `writeJSON` must keep
  its `json.NewEncoder(w).Encode(body)` inline rather than delegating to
  `writeJSONStatus` — chaining helpers degrades every response schema to a
  bare `object`.

## Patched apispec build

Upstream apispec (v0.3.5 and main @ cb336e36) needs two fixes for this
codebase, carried in `scripts/patches/apispec-fixes.patch` until merged
upstream:

1. A visited-set guard in `extractRouteChildren` — the tracker tree contains
   cycles here, which crashed the tool with a stack overflow.
2. Implicit-200 normalization in `buildResponses` — a body written without
   `WriteHeader` is a 200 per net/http semantics; unpaired bodies otherwise
   land in a `default:` slot (or duplicate/mislabel statuses).

`scripts/gen-openapi.sh` clones the pinned commit, applies the patch, and
caches the binary at `bin/apispec-patched`.

## Overlay for operations apispec cannot extract

`openapi-overlay.yaml` (repo root) is deep-merged into the generated spec as
the last step of `make openapi` (by `scripts/openapi-overlay`; a `null` value
deletes the key it overrides). Entries there are hand-maintained — keep them
in sync with their handlers when the wire contract changes.

Current entries:

- `POST /v1/server/player-sessions/verify` — apispec fails to traverse this
  handler's subtree (cause not yet isolated upstream), so its request body,
  200 response, and opaque 401 come from the overlay. Handler:
  `internal/httpapi/player_sessions_verify.go`.

## Known gaps

- `GET /v1/ws` — WebSocket upgrade endpoint; OpenAPI cannot describe the
  socket protocol, so it appears as a stub operation.
