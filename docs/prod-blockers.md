# Prod Release Blockers

## Goal

Fix the release-blocking issues found in prod review (`prod-release.md`, 2026-07-29/30) before
launch. Each milestone below is self-contained. A coding agent can execute a milestone without
extra context. This doc also folds in `docs/new_bugs.md` (the "[email protected]" defect is the
same as B5-T4 here).

## Decisions (agreed 2026-07-29)

- **Forgot password**: ship for BOTH auth stacks now — dashboard users and players.
- **Tenant disable**: block API keys and player traffic; keep tenant-admin control-panel login so
  admins can re-enable or export. A platform-admin disable also locks tenant admins out.
- **Player unlink**: non-destructive. Mark the link inactive; keep project data; a later
  re-invite restores the same data.
- **Unsubscribe**: platform-wide suppression table with a one-click signed link. All future
  invite email to a suppressed address is dropped.
- **Priority**: everything in this doc blocks launch. Only the matchmaking 429 `Retry-After`
  header moved to fast-follow (stays in `docs/new-follow-ups.md`).

## Milestones

### B1 — Session-state correctness bug (do first, small)

`GET /v1/game-session/{id}` returns `state: "open"` for sessions past `expires_at`, and the
response has no `expires_at` field. The count query and `GetGameSessionByJoinCode` already
filter `expires_at > now()`; only the direct GET diverges.

- [x] Add a failing test: a session with `expires_at` in the past must not read as `"open"` on
      direct-id GET. (Integration tests `TestGameSession_expired_session_not_open_on_get`,
      `TestGameSession_get_returns_expires_at_for_live_session`; unit `TestEffectiveState`.)
- [x] Fix expiry handling: `GetGameSession` already returned `expires_at`; the handler now maps
      it through `gamesession.EffectiveState` — an expired session reads `"expired"`, never
      `"open"`. No SQL change needed.
- [x] Add `expires_at` to the GET response (also populated on create and join, since the three
      share `gameSessionResponse`).
- [x] `make openapi` regenerated (sqlc unchanged); Go SDK sync stays deferred (see
      Out of Scope).

### B2 — Control-panel UI fixes

- [x] API keys page shows the key type: `ListAPIKeys` now returns `key_type`,
      `APIKeyView.KeyType` + `TypeLabel()`, new "Type" column in `APIKeysPage`.
- [x] Admin menu consistency. Root cause: every page built its own `AppNav` literal and most
      omitted `IsPlatformAdmin`. Fix: `requireSession` now stashes `navFacts` (admin flag +
      plugins presence) in the request context via `h.sessionContext`, and `appLayout` reads
      them through `AppNav.isPlatformAdmin(ctx)` / `pluginsEnabled(ctx)` — one check on every
      render path. Regression test
      `TestAppLayout_should_render_admin_menu_from_session_on_every_page`. This also fixes the
      Plugins nav entry, which only showed on the plugins page itself.
- [x] Tenant name on sub-pages: rate-limits subtitle now shows "name — tenant #id"
      (`rateLimitsView` loads it via `GetTenantFacts`); tenant settings already showed it.
- [x] Trimmed the server settings page: Mail and Network-and-secrets sections removed, the
      control-panel on/off row dropped, and the now-dead `ServerSettingsSnapshot` fields and
      badge helpers deleted (main.go assembly updated).
- [x] Feature Grants render as a table (Feature / Status / action) with small
      `secondary outline btn-inline` Enable/Disable buttons; Pico defaults.
      (Note: `docs/FRONTEND_STYLING.md` referenced here does not exist in the repo; followed
      `FRONTEND_GUIDELINES.md`.)

### B3 — Account features

#### B3a — Player unlink (non-destructive)

- [x] Tests first: `TestPlayerUnlink_non_destructive_blocks_auth_and_reinvite_restores`
      (integration, full flow) + template unit tests. Migration `0030_player_unlink` adds
      `project_players.unlinked_at`.
- [x] Unlink action on the account home ("Linked games" table) with a JS-less GET confirm
      page (`UnlinkProjectPage`) before the POST. Players can only unlink; linking stays
      admin-driven (invite flow unchanged).
- [x] `UnlinkPlayerFromAccount` extended (was dead code): clears the account link, stamps
      `unlinked_at`, bumps `session_epoch`, guarded by the owning account; the handler also
      revokes the player's game sessions. The row and its data stay. `LinkPlayerToAccount` /
      `BindPlayerLinkedEmail` clear `unlinked_at`, so a re-invite restores the same row.
- [x] Blocked traffic: `GetPlayerByEmail` and `GetSessionByRefreshHash` filter
      `unlinked_at IS NULL` (login + refresh), and the epoch bump kills live access tokens.
      External-id/custom-token identity is tenant-asserted and stays the tenant's call.

#### B3b — Forgot password (both stacks)

- [x] Migration `0031_password_resets`: two tables (`control_panel_password_resets`,
      `player_account_password_resets`) — sha-256 token hash, 1-hour expiry, single-use
      (`used_at`, atomic consume query).
- [x] Dashboard flow: `internal/controlpanel/password_reset.go` — forgot/reset pages next to
      login (same public, IP-rate-limited group), "Forgot password?" link on the login page.
- [x] Player flow: `internal/players/account_password_reset.go` — same shape, CSRF forms,
      "Forgot password?" link on the account login page.
- [x] Both flows: constant "If an account matches" response (asserted byte-identical in tests),
      per-IP rate limit via the existing auth limiter group, bcrypt on the new password,
      all sessions invalidated on reset (revoke + epoch bump on player accounts).
      Integration tests: `TestForgotPassword_control_panel_full_flow`,
      `TestForgotPassword_player_account_full_flow` (incl. single-use token, enumeration check,
      old-session revocation).
- [x] Zero-config: random DB-hashed tokens need no signing secret at all; uses the existing
      mailer + `BaseURL` (players.Config gained `BaseURL`, wired from the same env as the
      control panel's; empty falls back to a relative link, as elsewhere).

#### B3c — Tenant disable

- [x] Migration `0032_tenant_disable`: `tenants.disabled_at` + `disabled_by`
      ('tenant'|'platform', pair-check constraint).
- [x] API-key middleware: `GetAPIKeyByHash` joins the tenant's disabled state; a disabled
      tenant's keys get the same terse 403 as a revoked key (clear, non-enumerating), which
      blocks all player traffic.
- [x] Tenant-admin self-disable + re-enable on the tenant settings page ("Tenant status" card,
      confirm prompts). In-handler authz `canManageTenantLifecycle`: platform admin or
      membership admin/owner. Self-disable keeps control-panel access.
- [x] Platform-admin disable (same settings page; platform admins see every tenant).
      `disabled_by='platform'` locks tenant admins out of that tenant's control-panel pages
      (enforced in `requireTenantAccess`) — interpreted as tenant-page lockout, not a global
      login block, so admins of other tenants keep those. Re-enable: `EnableTenantByTenantAdmin`
      only undoes a self-disable (guarded in SQL); `EnableTenantByPlatformAdmin` undoes either.
- [x] Negative-authz tests: member disable → 403; tenant admin re-enable of a
      platform-disabled tenant → 403 (`TestTenantDisable_selfservice_blocks_keys_and_reenable_restores`,
      `TestTenantDisable_platform_disable_locks_out_tenant_admin`).

### B4 — Permissions clarity

- [x] Role explanations at the point of role selection (`InviteTeamPage`): one sentence per
      role under the Role select. (Admins manage everything in the tenant; members are
      read-only — matches the casbin rows: `role:tenant_admin` manage vs `role:analyst` read.)
- [x] Help page gained a "Team roles" section (`#roles`) with the same sentences (+ owner).
      The invite email now carries the role sentence inline plus a link to `help#roles`
      (readable after sign-in). Template tests cover both surfaces.

### B5 — Email fixes

- [x] **T1 — Names in invites.** Team invite subject/body name the tenant
      (`tenantDisplayName`); player invites name the game (`projectDisplayName`) and the
      inviter. Asserted in `TestInviteEmails_name_the_tenant_and_game_and_unsubscribe_suppresses`.
- [x] **T2 — Unsubscribe.** Migration `0034_email_suppressions` (citext-keyed, platform-wide).
      One-click signed link (HMAC over the address with the shared email signing key — no new
      secret, no login) in every invite email; signed-out confirm/done pages served at
      `/v1/unsubscribe` (mounted independently of the player site so the link always resolves;
      accepts the RFC 8058 one-click POST). Invite senders check `IsEmailSuppressed` and drop
      the email (invite row still created); the mailer stays a dumb transport and only gained
      `Message.ListUnsubscribe` → `List-Unsubscribe` + `List-Unsubscribe-Post` headers.
- [x] **T3 — Invite copy.** Rewritten: who invited, game name, clear accept CTA, what ggscale
      is, expiry at the bottom, unsubscribe footer.
- [x] **T4 — "[email protected]".** Code side done: the pages that render addresses
      (invite-accept, friends rows, account home, verify, the account nav menu) are wrapped in
      Cloudflare's `<!--email_off-->` escape, so obfuscation cannot rewrite them even while the
      zone setting is on. Ops half handed to the operator: disable Email Address Obfuscation
      (Scrape Shield) for the `ggscale.com` zone in the Cloudflare dashboard, then re-verify
      the three surfaces from `docs/new_bugs.md`.
- [x] **T5 — Friend invite links.** Investigated: confirmed no friend-invite email path exists
      (`friends.go` only matches existing accounts). Decision (b): no email is sent and the
      flash copy now says so — "your friend request will appear in their friends list. No email
      is sent…" — instead of implying a delivery.

## Review fixes (2026-07-29, post-B3 review)

All six findings from the B3 review are fixed:

1. **Forgot-password timing enumeration** — the account lookup, token mint, and SMTP delivery
   now run in a detached goroutine (`context.WithoutCancel` + 30s timeout) for every
   valid-looking address, so a known account answers in the same time as an unknown one.
   `mailer.Recorder` is now mutex-guarded with a `Snapshot()` accessor for the async tests.
2. **Outstanding reset links surviving a reset** — `InvalidateControlPanelPasswordResets` /
   `InvalidatePlayerAccountPasswordResets` burn every open token in the same transaction as
   any password change (reset flow and the control-panel password-change form). Tests mint two
   links, reset with the newer, and assert the older one is 410.
3. **Live WebSockets surviving disable/unlink** — `realtime.Options.Revalidate` re-checks the
   session epoch (now stashed in the playerauth context) and the tenant's disabled state on
   every heartbeat tick; a failure closes the socket. Wired in `mountRealtimeRoutes` with a
   new `Deps.RealtimeHeartbeat` override for tests. Integration tests open a socket, then
   disable the tenant / bump the epoch, and assert the socket closes.
4. **Non-atomic token consume** — both reset handlers now validate the password (incl.
   bcrypt's 72-byte cap on the dashboard) and hash it BEFORE the single transaction that
   consumes the token, sets the password, revokes sessions, and burns the other links. A 422
   no longer spends the link (asserted in tests).
5. **Platform disable vs. self-disable** — `DisableTenantByPlatformAdmin` promotes a
   self-disable to `disabled_by='platform'` in place, with no re-enable window; test extended.
6. **Unlink lookup masking DB errors** — `linkedProjectForRequest` returns errors separately;
   handlers answer 500 (logged) instead of 404.

## Review fixes, round 2 (2026-07-30)

1. **WS revalidation DB cost** — per-socket polling replaced by a per-process sweeper
   (`internal/httpapi/realtime_lifecycle.go`): one batched query per tenant-with-open-sockets
   per interval (`ListPlayerSessionEpochs` + disabled state in one tx), O(active tenants)
   regardless of CCU. The heartbeat check is now pure in-memory. The sweeper goroutine starts
   with the first socket and exits when none remain. `docs/capacity-and-launch.md` updated.
2. **Fail-open on DB errors** — error classification: definitive answers (revoked epoch,
   deleted player, disabled/missing tenant) mark the socket stale immediately; infrastructure
   errors stop advancing the connection's `lastVerified` stamp and a bounded grace
   (4 sweep intervals) closes unverifiable sockets. Unit tests cover stale, grace, and
   no-revive semantics.
3. **Untracked reset goroutines** — forgot-password delivery is now a durable River job
   (`password_reset_email`, worker in `internal/jobs/password_reset.go`). The handler inserts
   one job per valid-shaped request (constant work, anti-enumeration preserved) and the job
   survives deploys and restarts with River's bounded workers. The detached goroutine remains
   only as a documented fallback when River is unavailable (self-host without the queue).
4. **Reset-token retention/indexes** — migration `0033_password_reset_indexes` adds partial
   account indexes for the burn-all-open-tokens update; new `password_reset_gc` periodic job
   (24h) deletes rows a day past expiry. Covered by jobs integration tests.

## Review fixes, round 3 (2026-07-30)

1. **Silent revalidation failures** — a failed lifecycle sweep now logs one Warn line per
   tenant per interval (already aggregated; never per socket) and increments
   `ggscale_realtime_lifecycle_sweep_failures_total`, so a schema/permission regression is
   visible before the bounded grace starts closing sockets. Each lifecycle close logs once
   with its reason and increments `ggscale_realtime_lifecycle_closes_total`
   (revoked / unverifiable).
2. **Oversized reset addresses** — `validControlPanelEmail` now uses the shared
   `webutil.ValidateEmail` (RFC 5321 254-byte cap, no display names), matching the player
   surface, so an oversized address can never reach the River job table. Unit test for the
   cap + integration assertion that an oversized address gets the constant response with no
   delivery work started.

## Review fixes, round 4 (2026-07-30, post-B5 review)

1. **Unsubscribe rate limit** — `/v1/unsubscribe` now sits behind the same signed-out per-IP
   limiter (`AuthIPRate`/`AuthIPBurst`) as every other public surface: the signed token gates
   who can suppress an address, the limiter bounds how often a valid token can be replayed
   into database writes. Integration test asserts a 429 under replay.
2. **Header-safe names in mail subjects** — `headerSafeName` (control-character check via
   `SanitizeHeader`, 120-byte cap) guards the tenant/project names T1 put into invite
   subjects; a hostile or oversized name degrades to the generic wording instead of silently
   breaking delivery. Unit table test + integration test with a `Evil\nGame` project name.
   (The Prometheus emergency-admission alert finding is real but lives in the infra
   submodule — tracked separately.)

## Resolved (kept for the release record)

- **Matchmaking match-formation collapse under concurrent enqueue** — FIXED (commit `6d803f1`),
  deployed and verified 2026-07-29. Root cause: per-project open-session cap filled with
  unjoined matchmade sessions; capacity errors burned ticket attempts. Fix: capacity errors
  requeue penalty-free, 10-minute pending TTL on matchmade sessions, cap raised to 1000.
  Writeup: `results/matchmaking-formation-finding.md`.

## Files to Touch (summary)

- `internal/db/queries/game_session.sql`, `internal/httpapi/game_session.go`,
  `internal/gamesession/service.go` (B1)
- `internal/controlpanel/` — `templates.templ`, `key_management.go`, `settings.go`,
  `rate_limits.go`, `types.go`, `team_handlers.go`, `players.go` (B2, B4, B5)
- `internal/players/` — `account.go`, `account_templates.templ`, `friends.go`, `invite.go` (B3a,
  B3b, B5)
- `internal/controlpanel/login.go` + new reset handlers, `internal/db/queries/` (B3b)
- `internal/tenant/middleware.go` + new migrations under `db/migrations/` (next free number;
  `0030_` at time of writing) (B3c, B3b, B5-T2)
- `internal/mailer/` (headers only, B5-T2); Cloudflare dashboard (B5-T4, ops)

## Out of Scope / Follow-ups

- Matchmaking 429 `Retry-After` header — fast-follow, tracked in `docs/new-follow-ups.md`.
- Player password reset via game/SDK surfaces — web account flow only for now.
- Per-project email preferences page — the global suppression list ships first.
- SDK sync for the `expires_at` field on game-session GET — do with the next SDK sync pass.
