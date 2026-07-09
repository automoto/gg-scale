-- name: CreateAnonymousPlayer :one
INSERT INTO project_players (tenant_id, project_id, external_id)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1,
    $2
)
RETURNING id, external_id, created_at;

-- name: CreateEmailPlayer :one
INSERT INTO project_players (
    tenant_id, project_id, external_id, email, password_hash,
    email_verification_code_hash, email_verification_salt,
    email_verification_expires_at, email_verification_last_sent_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, $4, $5, $6, $7, now()
)
RETURNING id;

-- name: GetPlayerByEmail :one
-- Disabled accounts (disabled_at IS NOT NULL) are filtered out here so
-- /v1/auth/login behaves identically to an unknown email — same dummy
-- bcrypt + invalid_credentials response.
SELECT id, project_id, password_hash, email_verified_at
FROM project_players
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND email = $2
  AND deleted_at IS NULL
  AND disabled_at IS NULL;

-- name: GetPlayerVerificationState :one
SELECT
    id,
    email_verified_at,
    email_verification_code_hash,
    email_verification_salt,
    email_verification_expires_at,
    email_verification_attempts,
    email_verification_lifetime_attempts,
    email_verification_locked_until,
    email_verification_last_sent_at
FROM project_players
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND email = $2
  AND deleted_at IS NULL;

-- name: LockPlayerVerification :exec
UPDATE project_players
SET email_verification_locked_until = sqlc.arg(locked_until)::timestamptz
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg(id);

-- name: SetPlayerVerificationCode :exec
UPDATE project_players
SET email_verification_code_hash    = $3,
    email_verification_salt         = $4,
    email_verification_expires_at   = $5,
    email_verification_attempts     = 0,
    email_verification_last_sent_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND id = $2;

-- name: IncrementPlayerVerificationAttempts :one
UPDATE project_players
SET email_verification_attempts = email_verification_attempts + 1
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1
RETURNING email_verification_attempts;

-- name: ReservePlayerVerifyAttempt :one
-- Atomic check-and-bump (see ReserveControlPanelVerifyAttempt for the
-- TOCTOU explanation). Returns 0 rows when already at cap.
UPDATE project_players
SET email_verification_attempts = email_verification_attempts + 1,
    email_verification_lifetime_attempts = email_verification_lifetime_attempts + 1
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg('id')
  AND email_verification_attempts < sqlc.arg('max_attempts')::int
RETURNING email_verification_attempts, email_verification_lifetime_attempts;

-- name: ClearPlayerVerificationCode :exec
UPDATE project_players
SET email_verification_code_hash    = NULL,
    email_verification_salt         = NULL,
    email_verification_expires_at   = NULL,
    email_verification_attempts     = 0
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: MarkPlayerVerified :exec
UPDATE project_players
SET email_verified_at               = now(),
    email_verification_code_hash    = NULL,
    email_verification_salt         = NULL,
    email_verification_expires_at   = NULL,
    email_verification_attempts     = 0
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: GetPlayerByExternalID :one
SELECT id, project_id, email_verified_at
FROM project_players
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND external_id = $2
  AND deleted_at IS NULL;


-- name: CreateSession :one
INSERT INTO sessions (tenant_id, project_id, player_id, refresh_hash, expires_at)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, $4
)
RETURNING id, created_at;

-- name: GetSessionByRefreshHash :one
-- Joined to project_players so refresh fails for disabled / deleted accounts
-- even if the refresh token is still otherwise valid. revoked_reason lets the
-- refresh handler tell a replayed *rotated* token (theft) from a logged-out one.
SELECT s.id, s.player_id, s.project_id, s.expires_at, s.revoked_at, s.revoked_reason
FROM sessions s
JOIN project_players u ON u.id = s.player_id
WHERE s.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND s.project_id = sqlc.arg(project_id)
  AND s.refresh_hash = sqlc.arg(refresh_hash)
  AND u.deleted_at IS NULL
  AND u.disabled_at IS NULL;

-- name: RevokeSession :exec
-- Rotation path: the token is being superseded by a freshly-issued one, so a
-- later replay of this hash is a reuse (theft) signal.
UPDATE sessions
SET revoked_at = now(), revoked_reason = 'rotated'
WHERE id = $1
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND revoked_at IS NULL;

-- name: RevokeSessionByRefreshHash :one
-- Logout path: a later replay of this hash is a benign stale-client retry, not
-- reuse, so it must NOT trip the family kill.
UPDATE sessions
SET revoked_at = now(), revoked_reason = 'logout'
WHERE refresh_hash = $1
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND revoked_at IS NULL
RETURNING player_id;

-- name: RevokeActivePlayerSessions :execrows
-- Reuse-detection response: nuke every live session for the player in this
-- project. Paired with BumpPlayerSessionEpoch so outstanding access tokens die
-- at the epoch gate immediately, not just at TTL.
UPDATE sessions
SET revoked_at = now(), revoked_reason = 'reuse_detected'
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND player_id = sqlc.arg(player_id)
  AND revoked_at IS NULL;

-- name: GetTenantCustomTokenSecret :one
SELECT custom_token_secret
FROM tenants
WHERE id = current_setting('app.tenant_id', true)::bigint;

-- name: UpsertPlayerByExternalID :one
-- Custom-token flow: find existing player with this external_id under
-- (tenant, project) or create one. Idempotent across repeated calls.
INSERT INTO project_players (tenant_id, project_id, external_id)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2
)
ON CONFLICT (tenant_id, project_id, external_id)
    WHERE deleted_at IS NULL
DO UPDATE SET external_id = EXCLUDED.external_id
RETURNING id;
