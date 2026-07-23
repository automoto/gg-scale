# Changelog

All notable changes to ggscale are recorded here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project is
pre-1.0, so breaking changes may land in minor releases. Server and SDK (Go + C#) wire types are
released in lockstep.

## [Unreleased]

### Managed relay GA (in progress)

Managed TURN relay graduates from "built but shipped OFF/BYO-only" toward GA,
hosted by ggscale as a **dedicated relay VM per region**. Per-tenant enablement
is still gated by the `p2p_relay` feature grant. See `docs/relay-ga.md` (plan)
and `docs/relay-ops.md` (runbook).

#### Added

- **Standalone relay mode.** `ggscale-server relay` runs only the pion TURN
  listener (no DB/HTTP/matchmaker) for deployment on a dedicated relay VM, with
  an optional `RELAY_HEALTH_ADDR` serving `/healthz` + `/metrics`.
- **TURN/TCP and TURNS/TLS transports** (`RELAY_TCP_PORT`, `RELAY_TLS_PORT`,
  `RELAY_TLS_CERT_FILE`, `RELAY_TLS_KEY_FILE`) so clients behind UDP-blocking
  firewalls can reach the relay.
- **`RELAY_URLS`** — the TURN/TURNS URIs clients dial, now echoed in every
  issued credential set (previously always empty; credentials were undialable).
  Required when the credential issuer is enabled.
- **Zero-downtime secret rotation.** Credentials embed a key id; configure
  `RELAY_SHARED_SECRET_NEXT` alongside the active secret to rotate across a
  credential-TTL overlap window.
- **Per-player relay issuance rate limit** so one player can't drain a tenant's
  monthly allowance in a burst. New metric `ggscale_relay_issue_throttled_total`;
  relay nodes expose `ggscale_relay_up`.

#### Changed

- Removed the always-`null` per-peer `relay` field from the game-session peer
  response (server + Go/C# SDKs + OpenAPI); relay credentials come from
  `POST /v1/relay/credentials`, not the peer roster.

### Matchmaking GA

Matchmaking graduates from beta for the peer-to-peer paths (`match_only`,
`game_session`). `fleet_allocation` (dedicated servers) remains
entitlement-gated and is not part of this GA.

#### Breaking

- **One active ticket per player per project.** A player may hold at most one
  queued matchmaking ticket per project (enforced by a partial unique index).
  A second create while one is still queued now returns **HTTP 409** with a
  structured error carrying the active ticket id to cancel, instead of opening
  a second ticket. Multi-ticket queuing is removed.
- **`match_ready` realtime event replaced by `matchmaker_matched`.** The server
  emits only `matchmaker_matched`. Its payload is unified across modes and adds
  `host_player_id` (the P2P host for `match_only`/`game_session`) and per-member
  `attributes`. Consumers of the old `match_ready` event must migrate; the Go
  and C# SDK high-level helpers already parse the new event.
- **`MATCHMAKER_MAX_TICKETS_PER_PLAYER` removed.** The environment variable and
  its config/validation wiring are deleted; the one-active-ticket rule replaces
  it. Remove the variable from deployment configs.

#### Added

- Machine-readable `failure_reason` on failed tickets (`expired` |
  `attempts_exhausted`), surfaced in the ticket poll response.
- Poll-based match recovery: a committed match is retrievable by polling even
  when the realtime push is missed.
- Observability metrics for matchmaker queue health:
  - `ggscale_matchmaker_ticket_failures_total{reason}` — tickets that ended in
    `failed`, by reason.
  - `ggscale_matchmaker_time_to_match_seconds` — queued→matched latency
    histogram.
  - `ggscale_matchmaker_queue_depth{mode,region,game_mode}` and
    `ggscale_matchmaker_oldest_ticket_age_seconds{mode,region,game_mode}` —
    per-bucket gauges sampled on the sweep cadence (head-of-line-blocking early
    warning).

#### Migrations

- `0018`–`0021` add the one-active-ticket dedup + partial unique index (built
  `CONCURRENTLY`), the `failure_reason` column, and the match `host_player_id`
  column. `0021` builds its index in its own transaction; see the deploy
  runbook for the `CONCURRENTLY`-abort recovery.
