# Game-Server Session Verification (Pattern A)

## Why

Today, `doomerang-server` captures `req.GgscaleSessionToken` from
`JoinRequest` and stores it (`server/core/server.go` `spawnPlayer`), but
never validates it on join. The token is only exercised implicitly at
match end when `buildSubmitScoresHook` calls
`gg.Leaderboards.SubmitFor(ctx, tok, …)`. If the token is bogus or
forged, the player plays a full match under a fake identity and only
the score submission fails — silently. The architecture doc
(`gameserver.md` in doomerang-mp) lists four standard patterns; this
plan implements Pattern A — **validate on join via a synchronous call
to ggscale**.

Pattern B (signed allocation ticket, JWKS-verified by the server) is
the long-term answer but moves more pieces. Pattern A ships now,
closes the immediate identity gap, and doesn't preclude B — the
game-server still receives a session token; how it's verified changes
underneath. See [Out of scope](#out-of-scope) for the B comparison.

## Decisions

- **Game-servers authenticate their verification request with the
  server-tier API key.** The same `GGSCALE_SECRET_KEY` they already use
  for `Fleet.Register` / `Leaderboards.SubmitFor`. Not the player's
  session token. A game-server is a workload, not an end-user.
- **New endpoint: `POST /v1/end-users/verify`.** Request body
  `{session_token: "..."}`. Response `{user_id, external_id, email}`
  on 200, 401 with an opaque error message on
  invalid/expired/tampered token. `external_id` is the per-game stable
  identifier (Steam ID, anonymous UUID, etc.) — schema column on
  `end_users`. `email` is omitted when not set. No CORS —
  server-to-server only.
- **Underlying validation reuses `auth.Signer.Verify`.** Same HMAC
  primitive the `enduser.Middleware` already uses for browser/client
  session-token auth. No new crypto.
- **Reject-then-disconnect on invalid token.** `onJoinRequest` sends
  `JoinRejected{Reason: "invalid session"}` and lets the WebSocket
  close. No retry loop, no grace period — clients with a bad token
  must refresh through ggscale, not retry against the game-server.
- **No caching in v1.** A verify call is one extra round-trip per join,
  which already costs a WebSocket handshake. Revisit if a published
  load test shows the verify endpoint as a bottleneck.
- **Per-tenant rate limit on the verify endpoint.** Generous (1000
  rpm/key) but bounded so a misbehaving game-server can't probe
  arbitrary tokens. Reuses the existing `internal/ratelimit` machinery.
- **`GGSCALE_URL`-unset fall-through stays.** When the game-server runs
  without ggscale configured (local dev, `make run-server`), `onJoinRequest`
  skips verification — same way it already skips ggscale registration.

## Current state (ggscale audit, 2026-06-07)

| Item | State | Evidence |
|---|---|---|
| `auth.Signer.Verify` exists and verifies session JWTs | ✅ | `internal/auth/auth.go` (HMAC-SHA256, exp/jti claims) |
| `enduser.Middleware` uses Signer to inject UserID into ctx | ✅ | `internal/enduser/middleware.go:39` |
| Session-token middleware mounted on `/v1/leaderboards`, `/v1/matchmaker`, `/v1/storage`, etc. | ✅ | `internal/httpapi/router.go`, per-route mounts |
| API-key middleware for server-tier endpoints | ✅ | reused by `/v1/fleet/servers` write routes |
| `POST /v1/end-users/verify` endpoint | ❌ | this plan adds it |
| `EndUsers.VerifySession` SDK method | ❌ | this plan adds it (ggscale-go) |
| `onJoinRequest` calls verify before spawning the player | ❌ | doomerang-mp side; this plan wires it |
| Per-API-key rate limit on `/v1/end-users/verify` | ❌ | new limit using existing ratelimit cache |

## Implementation plan

### Phase A — ggscale `POST /v1/end-users/verify` endpoint

**Files:**
- New: `internal/httpapi/end_users_verify.go`
- New: `internal/httpapi/end_users_verify_test.go`
- Edit: `internal/httpapi/router.go` (mount inside the API-key block, NOT the session block)
- Edit: `internal/httpapi/deps.go` if a new dep is needed (signer is already in Deps via the enduser middleware wiring; reuse it)

**Tests first** (`end_users_verify_test.go`):

- `should_return_user_id_for_valid_session_token`
- `should_return_401_for_expired_token`
- `should_return_401_for_tampered_signature`
- `should_return_401_for_missing_or_empty_body`
- `should_return_401_when_token_signed_by_a_different_signer`
- `should_return_404_when_user_id_in_token_no_longer_exists` (deleted account)
- `should_require_api_key_auth` (call without API key → 401)
- `should_isolate_tenants` (server-tier key from tenant A verifying a token signed for tenant B → 401)

**Request:**
```json
POST /v1/end-users/verify
Authorization: Bearer <server-tier API key>
Content-Type: application/json

{"session_token": "<jwt>"}
```

**Response (200):**
```json
{"user_id": 12345, "external_id": "steam-76561198000000000", "email": "player@example.com"}
```

**Response (401):**
```json
{"error": "invalid session"}
```

The handler:
1. Parses body, requires non-empty `session_token`.
2. Calls `d.Signer.Verify(body.SessionToken)` — gets the user ID claim
   or `ErrTokenExpired`/`ErrTokenInvalid`.
3. Looks up the user in the DB to confirm they exist and the account
   isn't deactivated.
4. Confirms the user belongs to the same tenant as the API key
   (`TenantFromContext` matches the user's `tenant_id`).
5. Returns `{user_id, external_id, email}`.

Errors collapse to a single opaque 401 to avoid leaking
expired-vs-tampered distinctions to a hostile caller.

### Phase B — Rate limit `/v1/end-users/verify`

**Files:**
- Edit: `internal/httpapi/end_users_verify.go` (wrap handler with limiter)
- Edit: `internal/ratelimit/...` only if a new bucket key is needed

Per-API-key bucket. Default: 1000 rpm. Honest player joins generate a
trickle of verify calls; this caps a misbehaving game-server (or a
stolen key) from being used as an oracle to probe arbitrary session
tokens.

### Phase C — ggscale-go SDK `EndUsers.VerifySession`

**Files:**
- New: `endusers.go` (ggscale-go repo)
- New: `endusers_test.go`
- Edit: `client.go` (instantiate `EndUsers` on `NewClient`)

**Shape:**
```go
type EndUsersService struct {
    transport transport // existing pattern from FleetService
}

type EndUserVerifyResult struct {
    UserID     int64  `json:"user_id"`
    ExternalID string `json:"external_id"`
    Email      string `json:"email,omitempty"`
}

func (s *EndUsersService) VerifySession(ctx context.Context, sessionToken string) (*EndUserVerifyResult, error)
```

Uses `transport.Call` (the API-key path, not `callProtected` which
requires a session). Tests follow the same fake-transport shape as
`FleetService` tests.

Add to `*Client`:
```go
type Client struct {
    // existing fields…
    EndUsers *EndUsersService
}
```

### Phase D — doomerang-mp `onJoinRequest` integration

**Files:**
- Edit: `server/core/server.go` (`onJoinRequest`, `spawnPlayer`)
- Edit: `server/cmd/server/main.go` (pass the `*ggscale.Client` to the
  server so handlers can call `VerifySession` — today it's wired to the
  match-end hook only)

**Sequence inside `onJoinRequest`:**

1. Existing checks: not-pending → ignore; `draining` → reject; version
   mismatch → reject.
2. **New:** if a `*ggscale.Client` is wired and `req.GgscaleSessionToken
   != ""`:
   - Call `gg.EndUsers.VerifySession(ctx, req.GgscaleSessionToken)`
     with a short context timeout (e.g. 3 s).
   - On error: send `JoinRejected{Reason: "invalid session"}`, log, return.
   - On success: stash `result.UserID` and `result.ExternalID` for
     later use; continue to `spawnPlayer`.
3. Existing `spawnPlayer` path runs unchanged.

**Fall-through cases** (no rejection, no verification):
- ggscale not configured (no `*ggscale.Client`): skip — local dev.
- `req.GgscaleSessionToken == ""`: skip — anonymous play allowed when
  ggscale isn't gating it. Document this in `gameserver.md` so it's
  clear.

**Tests** (`server/core/server_join_verify_test.go`):

- `should_reject_join_when_session_token_invalid`
- `should_accept_join_when_session_token_valid`
- `should_accept_join_when_no_ggscale_client_wired` (local-dev path)
- `should_use_verified_user_id_for_match_end_submission` (stash and
  thread it through the leaderboard hook)
- `should_timeout_within_3s_if_ggscale_unreachable`

### Phase E — Update `gameserver.md` to reflect Pattern A is live

Add a "Session verification" section under "A player joining a match,
step by step" explaining that step 6 now includes a synchronous
`VerifySession` round-trip and a `JoinRejected{Reason: "invalid
session"}` failure mode. Cross-reference Pattern B as the planned
upgrade path.

## Out of scope (tracked for later)

- **Pattern B — signed allocation ticket.** Long-term replacement.
  ggscale matchmaker mints a short-lived JWT (audience-scoped to the
  allocated server, subject = user, exp ~10 min) and returns it
  alongside `match_address`. doomerang fetches ggscale's JWKS once at
  startup and verifies locally — no per-join network call. Lives in
  `docs/temp/backlog.md` until ranked is on the roadmap.
- **Caching VerifySession results.** Server holds a 5-minute LRU keyed
  on token → user-id. Saves a round-trip on reconnects within a match.
  Skipped for v1 because:
  - Negative-cache logic (cache "invalid" too?) is fiddly
  - Revocation invalidates the cache
  - The verify endpoint is one HMAC + one DB row by user-id, sub-1 ms
- **Anonymous play / "guest" tier.** Out of scope. v1 accepts empty
  tokens only when ggscale isn't wired (dev mode).
- **Mid-match token refresh.** A player whose token expires mid-match
  stays in the match; only the post-match `SubmitFor` fails. Acceptable
  for now.
- **Verification audit log.** Surfacing verify-call counters in the
  dashboard. Useful for ops but not blocking.

## Verification

| Phase | Done when |
|---|---|
| A | `go test ./internal/httpapi/... -run VerifyEndUser` green; handler returns 200 for valid, 401 for the four invalid cases, 401 for cross-tenant. |
| B | `go test ./internal/ratelimit/...` green for the new bucket key; integration test confirms 1001th call within 60 s gets 429. |
| C | `go test ./...` in ggscale-go green; fake-transport tests cover happy + 401 + transport-error. |
| D | `go test ./server/core/...` in doomerang-mp green; the existing drain + lifecycle tests still pass under `-race` + goleak. |
| E | `gameserver.md` updated; the join-sequence section calls out the new step. |

End state: a forged or stale session token gets `JoinRejected` at the
WebSocket handshake, before any game-state mutation. Honest tokens
flow exactly as they do today. ggscale gets one extra QPS per join
(bounded by the rate limit) and an opaque 401 is the only thing a
hostile caller can probe with.
