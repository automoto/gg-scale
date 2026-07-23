# Matchmaking GA readiness plan

Date: 2026-07-22
Status: M1–M4 done (server-side correctness + P2P, TDD, lint-clean). M5 (SDK
sync, separate repos) not started. M6 partial (short-commit metric + docs
done; e2e suite, remaining metrics, Grafana, changelog outstanding).

Fixes the release blockers from `archive/matchmaking-improvements.md` and makes
peer-to-peer matchmaking a first-class GA capability. Fleet
(`fleet_allocation`) is explicitly out of GA scope.

Conventions for implementing agents: TDD per `CLAUDE.md` (failing tests
first, testify `assert`, table tests where sensible). Never edit an applied
migration — always add a forward one. Do not reference this doc's milestone
numbers in code, comments, identifiers, or migration names.

## Decisions (locked 2026-07-22)

1. **Fleet skipped for GA.** `fleet_allocation` stays implemented and
   entitlement-gated (fleet key scope + `dedicated_servers` entitlement) but
   is documented as **beta**. No new fleet work here. The allocation-saga
   idempotency and match-bound server assignment findings
   (`archive/matchmaking-improvements.md` §4–§5) become the exit criteria of a
   future fleet-GA plan.
2. **P2P is the flagship GA path — no new mode.** `game_session` is already
   the P2P vehicle: the matchmaker creates a private session and
   `gamesession.MatchAdapter` already sets `HostPlayerID = players[0]`
   (`internal/gamesession/adapter.go`). GA work surfaces the host through
   the matchmaker wire contract and documents the handshake. `match_only`
   is the bring-your-own-signaling P2P path and also gains a host.
3. **One active ticket per player per project.** Multi-ticket support is
   removed (deliberate pre-1.0 break). A second create while one ticket is
   queued returns 409 with a structured code. This kills sibling-ticket
   arbitration and self-matching at the source; group formation additionally
   enforces player uniqueness as a defense-in-depth invariant.
4. **Matched peers see each other's `attributes`.** Roster entries gain the
   opaque `attributes` passthrough so `match_only` P2P can exchange lobby
   codes or endpoints with zero extra infrastructure. Peer visibility is
   documented and the field is size-capped at create.
5. **Host selection rule.** Host = the group's oldest ticket
   (`group[0]` — groups are built oldest-first, ties by ticket id). The
   longest-waiting player hosts. Deterministic, no config.

## Design reference

### One-active-ticket contract

- Enforcement is a partial unique index, not a check-then-insert count:
  `ON matchmaking_tickets (tenant_id, project_id, player_id) WHERE
  status = 'queued'`. Claimed tickets remain `status = 'queued'`, so a
  player mid-negotiation is still blocked from re-queuing — intended.
- `POST /v1/matchmaker/tickets` with an active ticket → 409 problem details
  with code `ticket_already_active` and the active ticket id. Remedy is
  cancel-then-recreate; SDKs surface a typed error.
- `MATCHMAKER_MAX_TICKETS_PER_PLAYER` (config, docs, `EnqueueRequest.MaxActive`,
  `ErrTicketLimit` path) is deleted.

### All-or-none commit contract

`Queue.CommitTickets` becomes transactional: it flips **all** requested
still-queued claim rows or **none**. `committed == len(group)` is the only
success. A short commit (a member cancelled/expired between claim and
commit) rolls back, returns a sentinel (`ErrShortCommit`), and the worker:

- drops the terminal tickets from the group (they are already settled),
- returns the survivors penalty-free via `ReturnUnmatched` so they rematch
  on the next pass without the canceller,
- emits no matched event; the orphan match row is GC'd by retention
  (existing `committed == 0` behavior),
- on the fleet path, additionally deallocates the orphan allocation
  (existing `deallocateOrphan`).

`committed == 0` (whole claim drifted) keeps its current handling.

### Structured failure reasons

New nullable `failure_reason` column on `matchmaking_tickets`, surfaced in
the poll response for `failed` tickets only. Initial values:

- `expired` — set by `ExpireMatchmakerTickets` (TTL sweep, currently flips
  to bare `failed`).
- `attempts_exhausted` — set by `ReleaseTickets` when
  `allocation_attempts` hits the cap.

Documented as an open enum in OpenAPI (more values may be added).

### Host designation and P2P wire changes

- `matchmaker_matches` gains nullable `host_player_id`; set for
  `match_only` and `game_session`, absent for `fleet_allocation`.
- `matchedPayload` (`matchmaker_matched` envelope) and the ticket poll
  response gain `host_player_id`.
- `RosterEntry` gains `attributes` (`json.RawMessage`, omitempty), so
  `users[]` in both the event and the poll response carries each member's
  opaque attributes.
- `game_session` invariant: the session's `HostPlayerID` and the match's
  `host_player_id` are the same player (adapter uses `players[0]`, worker
  uses `group[0].PlayerID` — worker passes the roster oldest-first, and a
  contract test pins the alignment).

Recommended P2P handshake to document (the `game_session` path):

1. All players queue `game_session` tickets.
2. `matchmaker_matched` (or the poll) delivers `session_id`, `join_code`,
   `host_player_id`, and the roster.
3. The host joins the session and registers its endpoint (existing session
   join/heartbeat flow); peers read the host's endpoint from the session.
4. Peers connect directly; ggscale is out of the data path.

`match_only` path: same event minus the session — peers exchange connect
info via roster `attributes` (e.g. a platform lobby id) or their own
signaling.

### Breaking changes to call out in the changelog (pre-1.0, deliberate)

- One active ticket per player per project; second create → 409;
  `MATCHMAKER_MAX_TICKETS_PER_PLAYER` removed.
- Ticket `attributes` become visible to matched peers via the roster.
- New fields: `host_player_id`, `failure_reason`, `users[].attributes`.

## M1 — One active ticket per player + unique-player invariant ✅

- [x] Tests first:
  - [x] enqueue rejects a second active ticket for the same player,
    returning the active-ticket error carrying the active id (MemQueue
    parity test; the pg unique-index concurrency case is an integration
    test left for the pgqueue suite).
  - [x] enqueue after the previous ticket reaches a terminal
    status (matched/cancelled) succeeds.
  - [x] groups (table test): two tickets sharing a `PlayerID` never land in
    the same group ("should_not_group_duplicate_player").
  - [x] httpapi: second create → 409 problem details, code
    `ticket_already_active`, active ticket id present; create succeeds
    after cancel.
- [x] Forward migration, two files: `0018_matchmaker_one_active_ticket`
  settles existing duplicates (flip all but the newest queued ticket per
  tenant/project/player to `cancelled`); `0021_matchmaker_one_active_index`
  then adds the partial unique index `CONCURRENTLY` so the build takes no
  blocking lock on the live table. Split because `CREATE INDEX CONCURRENTLY`
  cannot share a transaction with the dedup UPDATE.
- [x] Expired-but-unswept tickets don't block re-queuing: the one-active
  index predicate can't be time-aware, so `Enqueue` TTL-expires the player's
  stale queued ticket first (`ExpirePlayerQueuedTicket` in the same tx;
  MemQueue mirrors it in the guard). Claimed tickets stay blocking.
- [x] `Enqueue` (pgqueue): map the unique-violation to `TicketActiveError`
  (wraps `ErrTicketActive`); delete the `MaxActive` counting path and
  `ErrTicketLimit`. `CountQueuedTicketsForPlayer` query replaced by
  `GetQueuedTicketForPlayer`.
- [x] Delete `MatchmakerMaxTicketsPerPlayer` from config, validate, router
  Deps, main wiring, `.env.example`, and the `config-refactor.md` inventory.
- [x] httpapi create handler: `TicketActiveError` → 409 with structured code
  and active ticket id (`errors[].location=active_ticket_id`).
- [x] `fillGroup` (`internal/matchmaker/groups.go`): skip candidates whose
  `PlayerID` is already in the group (invariant holds even if the index is
  ever relaxed).
- [x] MemQueue parity for the one-active rule and error.
- [x] `make openapi` (409 rides the shared default error response, no spec
  delta); `make lint` clean on touched packages.

## M2 — All-or-none ticket commits ✅

- [x] Tests first:
  - [x] queue parity: `CommitTickets` where one id was cancelled after
    claim → no rows flipped (rolled back), `ErrShortCommit` + would-be
    count returned.
  - [x] worker: 4-player group, one cancels between claim and commit → no
    match event to anyone, no roster containing the canceller, three
    survivors rematch on the next pass with no attempt penalty
    (`TestWorkerShortCommitReturnsSurvivorsAndSuppressesEvent`).
  - [x] worker fleet path: short commit → allocation deallocated.
  - [ ] e2e: cancel racing finalization never yields a delivered roster
    containing the cancelled player (deferred to the M6 e2e suite).
- [x] `CommitTickets` (pgqueue): run in a transaction; if
  `rows != len(ticketIDs)` roll back and return `ErrShortCommit` with the
  would-be count. `Queue` interface contract comment updated.
- [x] `finalizeMatch` and `commitFleetAllocation`
  (`internal/matchmaker/worker.go`): success requires a full commit; on
  `ErrShortCommit`, return the survivors penalty-free (new scoped
  `ReturnTickets` — precise per-group return rather than the claim-wide
  `ReturnUnmatched`), skip the event (fleet: deallocate first). Kept
  `committed == 0` behavior.
- [x] Short-commit metric counter (`ShortCommitCounter`) alongside the
  existing match counter.
- [x] MemQueue parity (`CommitTickets` all-or-none, `ReturnTickets`).
- [x] `make lint` clean on the package.

## M3 — Structured failure reasons ✅

- [x] Tests first:
  - [x] TTL sweep flips to `failed` with reason `expired`.
  - [x] `ReleaseTickets` at `MaxAttempts` sets `attempts_exhausted`.
  - [x] Poll response carries `failure_reason` for failed tickets only
    (guarded on `StatusFailed`; `omitempty` hides it otherwise).
- [x] Forward migration (`0019_matchmaker_failure_reason`): nullable
  `failure_reason` text on `matchmaking_tickets` (CHECK on known values).
- [x] `ExpireMatchmakerTickets`, `ReleaseMatchmakerTickets`, and
  `SweepStaleMatchmakerClaims` SQL set the reason; sqlc regen; MemQueue
  parity (shared reason constants in `ticket.go`).
- [x] Poll handler + `matchmakerTicketResponse` field + OpenAPI (documented
  as an open string enum in the field comment); `make openapi`.
- [x] `make lint` clean.

## M4 — P2P first-class: host designation + peer-visible attributes ✅

- [x] Tests first:
  - [x] worker: `match_only` (and `game_session`) matches set
    `host_player_id` to the oldest ticket's player; fleet matches set none.
  - [x] contract test: the session's host head (`players[0]`, which the
    adapter uses as `HostPlayerID`) equals the match `host_player_id`
    (`TestWorkerGameSessionHostMatchesSessionHead` — worker-level, the
    seam that actually aligns the two).
  - [x] event tests: `host_player_id` and `users[].attributes` present and
    correct (poll surfacing shares the same `ticketResponse` mapping).
  - [x] httpapi: `attributes` above the 4 KiB cap rejected at create.
- [x] Forward migration (`0020_matchmaker_host_player`): nullable
  `host_player_id` on `matchmaker_matches` (roster is persisted JSON —
  `RosterEntry.Attributes` rides along).
- [x] `newMatch`: set `HostPlayerID` for non-fleet modes; populate
  `RosterEntry.Attributes` from each ticket.
- [x] `matchedPayload` + poll response: `host_player_id`.
- [x] Attributes size cap at ticket create (`maxAttributesBytes` = 4 KiB;
  no prior per-field cap existed).
- [x] Verified session reads already expose each active peer's `ip:port`
  (member-only) via `ListGameSessionPeers`, so peers retrieve the host's
  endpoint by matching `host_player_id` — no payload change needed.
- [x] Docs: P2P section in `wiki/features.md` (Multiplayer Connectivity)
  and a `wiki/architecture.md` matchmaker touch-up covering both handshakes
  and peer-visible attributes.
- [ ] Optional: host marker on the dashboard match view (skipped —
  optional).
- [x] `make openapi`; `make lint` clean; `go test ./...` green.

## M5 — SDK sync (Go + C#) on the GA contract ✅ (unit level)

Executed the matchmaking-relevant slice of `docs/sdk-sync-plan.md` with
these scope deltas:

- [x] Both SDKs model the GA additions: `host_player_id`,
  `failure_reason`, `users[].attributes` (roster), and the 409
  `ticket_already_active` typed error (Go `ErrTicketActive` +
  `(*Error).ActiveTicketID`; C# `IsTicketAlreadyActive` +
  `ActiveTicketId`). Both transports now parse Huma problem-details
  (`detail`/`errors`) alongside the legacy envelope.
- [x] High-level helpers consume `matchmaker_matched` with polling
  recovery: Go `WaitForMatch` and C# `WaitForMatchAsync` combine the
  realtime push with an authenticated poll loop and return a unified
  `MatchResult`/`MatchResult` for every mode (`RequestMatch` kept as a
  thin alias).
- [x] **New P2P connect helper** (the suggested item 1): Go `ConnectP2P` /
  C# `ConnectP2PAsync` wait for the match, fetch match-scoped relay
  credentials, and (game_session) join the session with the local
  endpoint — returning host flag + roster + relay + peers in one call.
- [x] **Relay match scoping** (the suggested item 2): server accepts an
  optional `match_id` on `POST /v1/relay/credentials` and verifies roster
  membership before issuing; both SDKs pass it (`WithMatch` /
  `GetCredentialsAsync(matchId)`).
- [x] Fleet coverage in the helpers is mode-gated (ConnectP2P rejects
  fleet matches), matching fleet's beta status.
- [ ] doomerang-mp is gone (checkout removed); the live downstream
  consumer is `gg-scale-discord`, which has **pre-existing non-matchmaking
  drift** (`Session.EndUserID` → `PlayerID`) blocking a build against the
  authoritative SDK. Its match_ready→result migration is written but
  parked behind that separate drift fix. See follow-up note below.
- [ ] Gate: cross-SDK integration against one pinned server image
  (sdk-sync-plan M5) — the immutable-image run is still to wire; unit +
  race + format suites are green in both SDKs.

**Downstream follow-up (out of matchmaking scope):** migrate
`gg-scale-discord` to the authoritative SDK — fix `EndUserID`→`PlayerID`,
re-point its `replace` from the stale `../ggscale-go` to
`/Users/mydev/code/ggscale-go`, then land the match_ready→`MatchResult`
waiter change (already drafted).

## M6 — GA hardening: e2e invariants, metrics, docs, launch gate (partial)

> Closeout plan with milestones + coding-agent TODOs:
> `docs/temp/matchmaking-ga-launch.md` (L1 metrics, L2 e2e suite, L3 Grafana,
> L4 release comms, L5 deploy/launch).

- [x] e2e invariant suite (server repo): duplicate-player attempt;
  expired-ticket re-queue (incl. claimed-then-expired still blocks);
  cancel-during-finalize; worker kill at the claim boundary (sweeper
  recovers within `MATCHMAKER_CLAIM_TTL`, no partial match); missed-push
  polling recovery; expired ticket surfaces `failure_reason`. Landed in
  `tests/integration/matchmaker/invariants_integration_test.go` (L2).
- [x] Short-commit counter wired (`ggscale_matchmaker_short_commits_total`).
- [x] Remaining metrics: per-bucket queue depth and oldest-ticket-age
  gauges (`ggscale_matchmaker_queue_depth`,
  `ggscale_matchmaker_oldest_ticket_age_seconds`), time-to-match histogram
  (`ggscale_matchmaker_time_to_match_seconds`), failure-reason counter
  (`ggscale_matchmaker_ticket_failures_total{reason}`). Sampler + observers
  wired through the `WorkerConfig`/`FailureRecorder` interfaces (L1).
- [ ] Small Grafana addition on the monitoring stack: queue depth,
  time-to-match p50/p95. (Lives on the monitoring host, outside this repo — L3.)
- [x] Env-var reference scrubbed for the removed knob (`.env.example`,
  `config-refactor.md`; no live-wiki reference existed).
- [x] Changelog with the three breaking changes — root `CHANGELOG.md`
  (decision 2026-07-23; establishes the repo convention).
- [x] `fleet_allocation` labeled beta in the wiki matchmaking docs.
  Control-panel copy check is moot: fleet is not part of GA (2026-07-22).
- [x] Updated the readiness + findings tables in
  `archive/matchmaking-improvements.md`.

## GA exit criteria

- A player appears at most once in any roster and cannot hold two queued
  tickets in a project.
- A partial commit never produces a match; a cancelled player never
  appears in a delivered roster; survivors rematch without penalty.
- Killing a worker at any boundary strands no ticket beyond the claim TTL
  and produces no partial match.
- Every failed ticket carries a machine-readable `failure_reason`.
- Every `match_only` and `game_session` result identifies the host, and
  matched peers can exchange connect info (session join or roster
  attributes) without third-party infrastructure.
- Go and C# high-level helpers complete through both push and polling for
  `match_only` and `game_session`.
- Fleet remains entitlement-gated and is labeled beta in all public docs.

## Deferred (not this plan)

- Fleet GA: idempotent allocation saga (intents, leases, reconciliation,
  worker-kill coverage) and match-bound server assignment with per-player
  connect tokens. Prerequisite for lifting the beta label.
- Parties / group tickets — first post-GA feature.
- Server-owned queues and authoritative property enrichment — prerequisite
  for marketing to ranked/competitive games.
- Head-of-line blocking (oldest-128 window) — M6 metrics decide when.
- P2P host migration / re-notification, backfill, ready-check, QoS
  latency vectors.

## Sequencing

M1 → M2 → M3 are server-side correctness and land in that order (M2's
tests assume M1's single-ticket world; M3 is independent and may run in
parallel with M2). M4 settles the wire contract. M5 starts only after M4
freezes the wire shape. M6 closes the gate. SDK tags and the server deploy
that removes `match_ready` consumers ship together per the release order
in `docs/sdk-sync-plan.md`.
