# Changelog

All notable changes to ggscale are recorded here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project is
pre-1.0, so breaking changes may land in minor releases. Server and SDK (Go + C#) wire types are
released in lockstep.

## [v0.9.6]

### Added

- **Per-project player data deletion with a grace period.** An explicit
  request disables the player in that project, revokes every session, and
  schedules a permanent purge after `PLAYER_DELETE_GRACE_PERIOD` (default 30
  days); data in other projects and the global account are untouched. Players
  request from the game API (`POST /v1/auth/delete`, with a credential-based
  `POST /v1/auth/delete/cancel`) or from the account pages; tenant and
  platform admins request and cancel from the control panel. The purge is an
  hourly job that hard-deletes the row (sessions, presence, leaderboard
  entries, storage, tickets, and invites cascade) while audit history is kept
  with the actor cleared. Migration `0043`.

### Security

- Upgrade the relay from Pion TURN `v3.0.3` to `v5.0.12`, removing the
  vulnerable `pion/dtls/v2` dependency path reported by GO-2026-4479. The
  per-player allocation limiter supports Pion v5's stable authenticated user
  IDs, while unsupported EVEN-PORT and RFC 6062 TCP relay allocations fail
  closed. TURN-over-TCP and TURNS remain supported as client transports, with
  Pion's TLS handshake deadline intact. Pion v5 auth events also make
  MESSAGE-INTEGRITY failures visible in the auth-failure metric.

## [0.9.2] - 2026-08-05

The GA release. Managed TURN relay and matchmaking graduate from beta, and the
API gap work lands in full: player identity, in-client auth flows, Steam
sign-in, account linking, a session browser, friend codes, remote config,
leaderboard periods, and a secret-key server tier. Migrations `0018`–`0042`
apply in order.

### Upgrade notes

- **Custom tokens now need a public key** (Ed25519 or RSA). Migration `0038`
  drops `tenants.custom_token_secret` with no compatibility path; every tenant
  using custom tokens must upload a key before its clients sign in again.
- **Docker fleet backend removed** — it was not safe for production. Migration
  `0035` drops its grants. Remove `DOCKER_*` and `GAME_SERVER_PUBLIC_IP` from
  deployment configs.
- **`MATCHMAKER_MAX_TICKETS_PER_PLAYER` removed** — the one-active-ticket rule
  replaces it.
- **Migration `0042` rewrites leaderboard entries** into one row per
  (leaderboard, player, period), keeping each player's best. Back up first.

### Breaking

- One active matchmaking ticket per player per project; a second create returns
  409 with the active ticket id to cancel.
- `match_ready` is replaced by `matchmaker_matched`, with a unified payload
  carrying `host_player_id` and per-member `attributes`.
- `score` is required on both leaderboard submit routes (422 when absent), and
  `sort_order` locks once a board has entries.
- The always-`null` per-peer `relay` field is gone from the game-session peer
  response; credentials come from `POST /v1/relay/credentials`.

### Added

**Relay** — standalone `ggscale-server relay` mode serving `/healthz` and
`/metrics`; TURN/TCP and TURNS/TLS transports; `RELAY_URLS` and
`RELAY_STUN_URLS` echoed in every credential set; zero-downtime secret rotation
through a key id and `RELAY_SHARED_SECRET_NEXT`; a global allocation cap,
per-player issuance and allocation limits, and a private-peer filter
(`RELAY_ALLOW_PRIVATE_PEERS`). Metrics: `active_allocations`,
`allocations_rejected_total`, `auth_failures_total`, `alloc_throttled_total`,
`peer_rejected_total`, `issue_throttled_total`, `up`. See `docs/relay-ga.md`
and `docs/relay-ops.md`.

**Auth and identity** — display names with public and batch player lookup;
in-client password reset by emailed code, verification resend, change-password
and self-disable; anonymous-to-registered linking by email or Steam; native
Steam sign-in verified against Valve; friend codes with regenerate and resolve.
Provider credentials are sealed at rest (`CREDENTIAL_ENC_KEY`, auto-generated
when unset). See `docs/account-linking.md` and `docs/steam-auth.md`.

**Discovery and live-ops** — `GET /v1/game-sessions` public session browser
(cursor paged, heartbeat-driven liveness, private sessions never listed);
per-project remote config on `GET /v1/config` with ETag/304, readable with a
project key before login; peer-to-peer signaling on
`/v1/game-session/{id}/signals`. See `docs/session-browser.md` and
`docs/remote-config.md`.

**Leaderboards** — score operators (`best`/`set`/`incr`), per-score and
per-board metadata, calendar-aligned periods with a leader-elected archive job
and history endpoints, optional per-period attempt caps, opt-in client
submissions with a per-player rate limit and score bounds, plus board discovery
and friends views. See `docs/leaderboards.md`.

**Server tier** — secret-key routes to submit a score for a player and to
read, write and list a player's storage with no player session. See
`docs/server-tier.md`.

**Administration** — tenant self-disable and platform disable; forgot-password
for control-panel and player web sessions; player-initiated account unlink;
one-click invite-email unsubscribe with a suppression list; per-tenant quota
overrides and `quota_override` change requests; secret API keys manageable by
tenant admins.

**Matchmaking** — machine-readable `failure_reason`, poll-based match recovery,
and queue-health metrics (`ticket_failures_total`, `time_to_match_seconds`,
`queue_depth`, `oldest_ticket_age_seconds`).

**Build** — multi-architecture Docker images (`linux/amd64`, `linux/arm64`).

### Changed

- Match formation no longer collapses under concurrent enqueue: capacity errors
  requeue penalty-free, matchmade sessions start on a 10-minute pending TTL, and
  the per-project open-session cap rose from 100 to 1000. A 50-client load test
  went from 2 matched tickets to 1,755.
- Per-player cap on unclaimed fleet allocations
  (`MATCHMAKER_MAX_UNCLAIMED_FLEET_ALLOCS`, default 3); over the cap returns 429.
- `api_keys.key_type` defaults to `publishable`, so a forgotten `key_type` no
  longer mints a limiter-exempt key.
- `RELAY_PUBLIC_IP` must be IPv4, and a half-set relay port range fails startup.
- Dropped the matchmaker's `pg_notify` trigger (a cluster-global lock held
  through the commit fsync) and replaced the `FOR UPDATE` + `COUNT(*)` player
  quota gate with a maintained counter. Both serialized every enqueue or
  anonymous sign-in.

### Fixed

- The relay peer filter admitted CGNAT, multicast and broadcast peers.
- The fleet allocation cap drifted and orphan match rows leaked.
- A NUL byte in a storage key or leaderboard name returned 500; control
  characters, overlong keys and an unbounded `key_prefix` are now rejected.
- `GET /v1/game-session/{id}` reported expired sessions as `open`.
- Password-reset recovery could dead-end at the lifetime attempt cap.
- Saving a custom-token public key always returned 404 (bootstrap scope vs RLS).
- The custom-token form echoed a rejected private key, and `make seed` failed on
  the leaderboard and matchmaking unique indexes.

### Security

- The relay resolves the HMAC password before touching the per-player limiter,
  caps the bucket map, and rate limits at pion's allocation boundary, which is
  reached only after MESSAGE-INTEGRITY succeeds. Forged traffic cannot move a
  victim's budget.
- `PATCH /v1/profile` no longer re-sends verification mail for an unchanged,
  already-verified address, and it honors the per-recipient cooldown.
- Password reset deletes trusted-device rows, so a stale remembered-device
  cookie cannot skip 2FA.
- Re-auth credential reads and client-submission policy checks moved to the
  primary pool; replica lag must not admit a replaced password or a score the
  board just forbade.
- Fleet heartbeat and server list check `FeatureDedicatedServers`.
- grpc → v1.82.1, x/text → v0.39.0.

### Removed

- Docker fleet backend (`internal/fleet/docker`, `compose/fleet-docker.yml`, and
  all `DOCKER_*` and `GAME_SERVER_PUBLIC_IP` settings).
- `compose/fleet-agones.yml` and the fleet test scripts, moved to their own repo.

### Migrations

- `0018`–`0021` — one-active-ticket index (built `CONCURRENTLY`),
  `failure_reason`, match `host_player_id`.
- `0022`–`0026` — game-session signaling; drop the matchmaker notify trigger;
  tenant-admin secret keys; `key_type` default.
- `0027`–`0035` — player-count counter; quota overrides and change requests;
  player unlink; password resets; tenant disable; email suppressions; drop the
  Docker fleet grants.
- `0036`–`0042` — remote config; Steam config; custom-token public key; friend
  codes; in-client player password reset; server storage policy; leaderboard
  rewrite.
