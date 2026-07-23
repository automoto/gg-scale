# Go and C# SDK contract sync plan

Status: planned 2026-07-16; implementation not started.

## Goal

Bring the Go and C# SDKs up to the current `ggscale` `/v1` contract, make the
server-generated OpenAPI document a dependable source of truth, and add drift gates
so server and SDK changes cannot silently diverge again.

The SDKs remain hand-written. Generated clients would conflict with the Go SDK's
idioms and the C# SDK's zero-runtime-dependency, Unity, AOT, and custom-JSON
constraints. OpenAPI is the contract and coverage input, not a replacement for the
public SDK designs.

## Repositories and baseline

Use these as the authoritative working copies:

- Server/spec: `/Users/mydev/code/ggscale`
- Go SDK: `/Users/mydev/code/ggscale-go`
- C# SDK: `/Users/mydev/code/gg-scale-sdk/gg-scale-sdk-C-Sharp`

Do not update `/Users/mydev/code/gg-scale-sdk/ggscale-go`; it is a stale second
checkout of the same remote. It is at `b9a2229`, while the authoritative checkout is
at `efb304f` and is the path named by the C# repository's own instructions.

Baseline findings from 2026-07-16:

- A fresh server-side generation is byte-for-byte identical to the checked-in
  `openapi.yaml`. The current document contains 38 paths, 46 operations, and 50
  component schemas.
- The C# `docs/openapi.yaml` is an exact copy of server commit `77edcd0` from
  2026-07-06. It predates the Huma migration, canonical-path cleanup, typed schemas,
  matchmaker rebuild, and storage-list change.
- The Go SDK has no vendored OpenAPI snapshot. Its current HEAD is also from
  2026-07-06.
- The authoritative Go checkout has existing uncommitted CI, quickstart, Makefile,
  and integration-compose edits. Preserve and reconcile them; they are not part of
  this plan's clean baseline.
- The C# repository has no commits yet and its entire implementation is untracked.
  Establish a baseline commit before applying contract-sync changes so the update is
  reviewable.

## Confirmed contract drift

### Critical: matchmaker rebuild

Both SDKs still model the original fleet-only request and wait for the deleted
`match_ready` event. The server now emits only `matchmaker_matched`; the current
high-level `RequestMatch` helpers can therefore wait indefinitely even when a match
has completed.

The SDKs need the current contract:

- Result modes: `match_only`, `game_session`, and `fleet_allocation`.
- Request fields: `mode`, `fleet`, `region`, `allow_cross_region`, `game_mode`,
  `min_count`, `max_count`, `count_multiple`, `query`, `string_properties`,
  `numeric_properties`, and `attributes`.
- Ticket/result fields: `match_id`, `match_address`, `protocol_hint`, `session_id`,
  `join_code`, `users`, `matched_at`, and `expires_at`, in addition to the normalized
  request fields.
- Roster entries: `player_id`, `region`, `string_properties`, and
  `numeric_properties`.
- Realtime payload: `ticket_id`, `match_id`, `mode`, mode-specific connection/session
  fields, and the roster.
- `GET /v1/matchmaker/tickets/{id}` is the recovery path for missed WebSocket
  delivery and claims an unexpired allocation for a polling client.

### Breaking: storage list is metadata-only

`GET /v1/storage/objects` no longer returns `value`. Each item contains `key`,
`version`, `updated_at`, and `size_bytes`. GET and PUT still return the full object.

Both SDKs currently use their full storage-object type for list items. Introduce a
distinct metadata/list-item type and change the page's item collection to that type.
This is an intentional pre-1.0 breaking change.

### Errors: SDK parsers predate Huma problem details

Most handlers now return `application/problem+json` with `title`, `status`, `detail`,
and optional validation `errors`. The SDKs currently parse the older
`error`/`message` envelope. Rate-limit and deadline middleware still emit the older
shape, and a few authentication/middleware failures remain plain text or deliberately
opaque.

Before release, make the server contract explicit:

- Canonical errors use problem-details fields plus optional stable extensions:
  `code`, `retry_after_seconds`, and `current_version` where the server actually has
  those values.
- Move rate-limit/deadline/middleware writers to the canonical envelope.
- Keep the player-session verification 401 deliberately opaque and document its
  separate `{ "error": "invalid session" }` schema.
- During the transition, both SDKs parse canonical problem details, the legacy JSON
  envelope, and plain text. `Retry-After` continues to override a body value.
- Do not promise `ConflictVersion` unless the server returns and documents it. Keep
  the SDK field for compatibility, but document it as optional until that server
  extension is implemented.

### No SDK wire change

The remaining code-review fixes are server, deployment, or behavioral changes. In
particular, T11's Huma body ceiling is runtime metadata and is excluded from OpenAPI.
T1 refresh-token single use and T7 match-allocation leases merit integration coverage,
but do not add request fields by themselves.

## Milestone 0 — establish reviewable baselines

- [ ] In the authoritative Go checkout, inventory the existing dirty files and either
  land them separately or explicitly carry them into the SDK branch without rewriting
  unrelated work.
- [ ] Record the Go baseline tag/commit and current public API with `go doc` or an
  equivalent API snapshot.
- [ ] In C#, make an initial baseline commit of the existing SDK before contract-sync
  work. Confirm secrets, build output, and local integration data are ignored.
- [ ] Confirm whether C# `0.1.0` has ever been published. If not, the synchronized SDK
  can be its first `0.1.0`; if it has, use `0.2.0` for the breaking update.
- [ ] Mark the nested stale Go checkout as non-authoritative in local contributor docs
  or remove it in a separate housekeeping change.

Acceptance:

- Each implementation change has a clean, attributable diff from a known baseline.
- No existing user changes in the Go checkout are lost.

## Milestone 1 — harden the server OpenAPI contract

### Generation and drift gates

- [ ] Add `make openapi-check`: generate to a temporary file and fail on any diff from
  root `openapi.yaml` without modifying the worktree.
- [ ] Add `openapi-check` to `make check` and Linux CI.
- [ ] Add a pinned OpenAPI 3.1 validation step. Pin the validator and its checksum or
  container digest; do not download an unversioned `latest` tool in CI.
- [ ] Extend OpenAPI tests beyond path presence:
  - unique and stable operation IDs;
  - canonical paths without trailing slashes;
  - expected API-key versus player-session security per operation;
  - request parameter names and requiredness;
  - success status and response schema;
  - the intentionally opaque verify response and WebSocket stub.

### Semantic audit

Audit all 46 operations against handler and integration-test behavior. For each one,
check method/path, security, path/query/header parameters, request content type,
required/nullable fields, validation bounds, success status/body, error statuses, and
response field requiredness. Correct the Go operation metadata/types first, then
regenerate; never hand-edit generated YAML.

Priority audit areas:

- matchmaker mode-dependent fields, defaults, count constraints, roster, and polling;
- storage full-object versus metadata-only list schemas;
- canonical path parameters and URL escaping;
- auth response/status shapes and single-use refresh behavior;
- rate-limit/deadline/problem-details responses;
- nullable optional fields in profile, friends, presence, invites, relay, and game
  sessions;
- server-tier versus player-tier security requirements.

### Error contract

- [ ] Define one canonical problem-details response type and writer for Huma handlers
  and HTTP middleware.
- [ ] Document optional machine fields only where emitted.
- [ ] Add live handler tests for problem details, rate limiting, request deadlines,
  storage conflicts, and opaque verification failures.
- [ ] Regenerate `openapi.yaml` and rerun the semantic audit.

Acceptance:

- `make openapi` produces no diff immediately after generation.
- CI fails if handler metadata changes without a regenerated spec.
- The checked-in document validates as OpenAPI 3.1 and matches representative live
  responses, including errors.
- No hand-maintained SDK appendix contradicts the authoritative spec.

## Milestone 2 — vendor and track the contract in each SDK

- [ ] Copy the reviewed server `openapi.yaml` into `docs/openapi.yaml` in both SDKs.
- [ ] Add a small provenance file containing the source server commit and SHA-256 of
  the copied spec.
- [ ] Add `scripts/sync-openapi` and `scripts/check-openapi` commands that accept an
  explicit server checkout path. They must fail clearly when the source differs and
  never fetch an unpinned spec from the network.
- [ ] Add an SDK coverage manifest mapping every operation ID to a public SDK method,
  an internal transport feature, or an explicit intentional exclusion. Compare the
  manifest to the vendored spec in CI so a new server operation cannot go unnoticed.
- [ ] Treat the WebSocket message schema as a companion contract because OpenAPI
  cannot fully describe it. Keep its envelope and `matchmaker_matched` payload next to
  the vendored spec and cover it with fixtures in both SDKs.

Acceptance:

- Both SDK repositories contain the exact same authoritative spec bytes and source
  provenance.
- Their CI detects a changed operation set or stale spec snapshot.
- Every operation has an explicit coverage decision.

## Milestone 3 — update the Go SDK, tests first

### Transport and errors

- [ ] Add fixtures for canonical problem details, validation errors, legacy rate-limit
  JSON, opaque verify JSON, and plain text.
- [ ] Parse `detail` into `Error.Message`, optional `code` into `Error.Code`, and retain
  legacy parsing. Preserve status-based `errors.Is` behavior and `Retry-After` priority.
- [ ] Correct error documentation that currently implies every response supplies a
  code or conflict version.

### Storage

- [ ] Add `StorageObjectMetadata` (final name may follow existing Go naming) with
  `Key`, `Version`, `UpdatedAt`, and `SizeBytes`.
- [ ] Change `ObjectPage.Items` to the metadata type. Keep `Object` for GET/PUT.
- [ ] Update unit and live integration tests to assert `size_bytes` and that list
  payloads do not contain/require `value`.

### Matchmaker and realtime

- [ ] Add string-backed mode constants and the complete request fields.
- [ ] Add the complete ticket/result and roster fields, preserving unknown JSON fields
  for forward compatibility where practical.
- [ ] Replace `MatchReady`/`match_ready` handling with a unified match result parsed
  from `matchmaker_matched`.
- [ ] Change the high-level request helper to return the unified result for every mode,
  not only a fleet address. Document this as a pre-1.0 break.
- [ ] Make recovery deterministic: combine WebSocket delivery with periodic authenticated
  ticket polling, or expose a shared `WaitForMatch` helper used by both realtime and
  polling callers. A dropped push must still return the persisted match before TTL.
- [ ] On cancellation, best-effort cancel only queued tickets; do not turn an already
  matched/recoverable result into a false failure.

### Go SDK hygiene

- [ ] Centralize the SDK version/User-Agent and bump the release to `v0.3.0`.
- [ ] Correct README dependency claims: realtime currently uses
  `github.com/coder/websocket`, so the SDK is not standard-library-only.
- [ ] Add a changelog and migration examples for storage-list and matchmaking breaks.

Acceptance:

- Unit tests cover every new field, mode, event, error shape, and polling transition.
- `make check` and `go test -race ./...` pass.
- The public API coverage manifest matches all 46 operations or records an intentional
  exclusion.

## Milestone 4 — update the C# SDK, tests first

Keep all existing repository constraints: `netstandard2.1;net8.0`, zero core runtime
dependencies, custom JSON, nullable enabled, AOT/IL2CPP-safe code, XML docs on every
public member, `CancellationToken` on async APIs, and `.ConfigureAwait(false)`.

### Transport and errors

- [ ] Port the same canonical/legacy error fixtures used by Go.
- [ ] Parse problem `detail`, optional extensions, validation errors, legacy envelopes,
  and plain text into `GGScaleException` without reflection or new core dependencies.
- [ ] Align documentation for optional `Code` and `ConflictVersion`.

### Storage

- [ ] Add a dedicated `StorageObjectMetadata` list-item type with `SizeBytes`.
- [ ] Change `StoragePage.Items` to that type; retain `StorageObject` for GET/PUT.
- [ ] Update unit and integration tests for the metadata-only response.

### Matchmaker and realtime

- [ ] Add the three modes, complete request options, complete ticket/result DTOs, and
  roster DTOs with explicit custom-JSON conversion.
- [ ] Replace `match_ready` with `matchmaker_matched` and return a unified match result
  from `RequestMatchAsync`.
- [ ] Implement the same polling recovery semantics as Go so both SDKs behave alike
  when WebSocket delivery is missed.
- [ ] Keep public naming and cancellation behavior idiomatic for C# rather than copying
  Go signatures mechanically.

### C# SDK hygiene

- [ ] Replace the stale spec and retire duplicated payload descriptions from Appendix A
  once the audited OpenAPI schema covers them; retain only WebSocket/behavioral notes.
- [ ] Complete package metadata, changelog, migration guide, and version consistency
  between the project and `SdkVersion`.
- [ ] Update `docs/temp/mvp.md` after the sync milestone, as required by the repository.

Acceptance:

- `make lint`, `make build`, and `make test` pass warning-free on Linux.
- Both target frameworks build and the core gains no runtime package dependency.
- The C# public behavior and fixtures match the Go reference without sacrificing C#
  conventions.

## Milestone 5 — cross-SDK integration against the exact server revision

Do not validate against an implicit Docker Hub `latest`; both current SDK integration
stacks can silently test a stale image.

- [ ] Build one local server image tagged with the audited server commit SHA.
- [ ] Make both integration runners require or report `GGSCALE_SERVER_IMAGE` and support
  `pull_policy: never` for the local contract run.
- [ ] Run both suites against that same image and equivalent seed data.
- [ ] Add/extend end-to-end cases for:
  - storage CRUD/OCC plus metadata-only list and `size_bytes`;
  - a storage value above 1 MiB when the configured project/platform limit permits it;
  - problem-details and legacy/middleware error parsing;
  - refresh-token rotation: the old token fails after one successful rotation;
  - `match_only` realtime completion with the full roster;
  - missed realtime delivery recovered by polling;
  - `game_session` returning `session_id`/`join_code`;
  - `fleet_allocation` returning address/protocol when a fleet backend fixture is
    available.
- [ ] Compare captured requests/responses between Go and C# for the same scenarios.

Acceptance:

- Go and C# integration suites pass against the same immutable server image.
- No test depends on whatever image happens to be published as `latest`.
- Realtime and polling flows both complete and return equivalent results.

## Milestone 6 — release and follow-through

Release order:

1. Land server contract fixes, regenerated OpenAPI, and drift CI.
2. Land and tag Go SDK `v0.3.0` with migration notes.
3. Land C# SDK and publish its first synchronized version (`0.1.0` if unpublished,
   otherwise `0.2.0`).
4. Update downstream consumers, especially `doomerang-mp`, from `match_ready` to the
   new result model.
5. Mark the SDK follow-ups complete in `docs/code-review-fixes.md` and
   `archive/temp/matchmaking-handoff.md` only after both releases and their immutable-image
   integration runs pass.

Release gates:

- Linux-only CI is green in all three repositories.
- Spec provenance in both SDK tags points to the released/deployed server contract.
- Changelogs call out the two breaking changes: storage list items and matchmaking.
- Examples cover at least one polling-safe match flow and the metadata-list/Get pattern.
- Package versions, User-Agent versions, Git tags, and published artifact versions agree.

## Work that stays out of this plan

- Generated SDK replacement projects.
- Control-panel/admin APIs.
- Deferred server matchmaker features such as parties, override hooks, or
  socket-required disconnect cancellation.
- Updating `doomerang-mp` itself; it is a downstream task after both SDKs land.
- C# engine packaging unrelated to validating the synchronized core contract.

