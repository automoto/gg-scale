-- Global player accounts. Platform-global (no tenant RLS): every query here is
-- run through db.Pool.BootstrapQ. See docs/temp/player-accounts.md.

-- name: CreatePlayerAccount :one
-- Creates an unverified account with its first verification code inlined
-- (mirrors CreatePlayerEndUser). Fails on the UNIQUE email constraint if an
-- account already exists.
INSERT INTO player_accounts (
    email, password_hash, display_name,
    email_verification_code_hash, email_verification_salt,
    email_verification_expires_at, email_verification_last_sent_at
)
VALUES (
    sqlc.arg(email), sqlc.arg(password_hash), sqlc.narg(display_name),
    sqlc.arg(code_hash), sqlc.arg(code_salt), sqlc.arg(expires_at), now()
)
RETURNING id, email::text AS email, created_at;

-- name: GetPlayerAccountByEmail :one
SELECT
    id,
    email::text AS email,
    password_hash,
    display_name,
    email_verified_at,
    disabled_at,
    session_epoch
FROM player_accounts
WHERE email = sqlc.arg(email);

-- name: GetPlayerAccountByID :one
SELECT
    id,
    email::text AS email,
    display_name,
    email_verified_at,
    disabled_at,
    session_epoch,
    created_at
FROM player_accounts
WHERE id = sqlc.arg(id);

-- name: GetPlayerAccountVerificationState :one
SELECT
    id,
    email::text AS email,
    email_verified_at,
    email_verification_code_hash,
    email_verification_salt,
    email_verification_expires_at,
    email_verification_attempts,
    email_verification_lifetime_attempts,
    email_verification_locked_until,
    email_verification_last_sent_at
FROM player_accounts
WHERE id = sqlc.arg(id);

-- name: SetPlayerAccountVerificationCode :exec
UPDATE player_accounts
SET email_verification_code_hash    = sqlc.arg(code_hash),
    email_verification_salt         = sqlc.arg(code_salt),
    email_verification_expires_at   = sqlc.arg(expires_at),
    email_verification_attempts     = 0,
    email_verification_last_sent_at = now(),
    updated_at                      = now()
WHERE id = sqlc.arg(id);

-- name: ReservePlayerAccountVerifyAttempt :one
-- Atomic check-and-bump; returns 0 rows when already at the per-code cap.
UPDATE player_accounts
SET email_verification_attempts          = email_verification_attempts + 1,
    email_verification_lifetime_attempts = email_verification_lifetime_attempts + 1
WHERE id = sqlc.arg(id)
  AND email_verification_attempts < sqlc.arg(max_attempts)::int
RETURNING email_verification_attempts, email_verification_lifetime_attempts;

-- name: LockPlayerAccountVerification :exec
UPDATE player_accounts
SET email_verification_locked_until = sqlc.arg(locked_until)::timestamptz
WHERE id = sqlc.arg(id);

-- name: MarkPlayerAccountVerified :exec
UPDATE player_accounts
SET email_verified_at            = now(),
    email_verification_code_hash = NULL,
    email_verification_salt      = NULL,
    email_verification_expires_at = NULL,
    email_verification_attempts  = 0,
    updated_at                   = now()
WHERE id = sqlc.arg(id);

-- name: SetPlayerAccountPassword :exec
-- Password change bumps session_epoch so every outstanding account session is
-- invalidated on its next request.
UPDATE player_accounts
SET password_hash  = sqlc.arg(password_hash),
    session_epoch  = session_epoch + 1,
    updated_at     = now()
WHERE id = sqlc.arg(id);

-- name: SetPlayerAccountDisabled :exec
-- Platform-level disable. Bumps session_epoch to kill live sessions.
UPDATE player_accounts
SET disabled_at   = now(),
    session_epoch = session_epoch + 1,
    updated_at    = now()
WHERE id = sqlc.arg(id)
  AND disabled_at IS NULL;

-- name: SetPlayerAccountEnabled :exec
UPDATE player_accounts
SET disabled_at = NULL,
    updated_at  = now()
WHERE id = sqlc.arg(id);

-- name: CreatePlayerAccountSession :one
INSERT INTO player_account_sessions (
    player_account_id, refresh_hash, session_epoch, expires_at
)
VALUES (
    sqlc.arg(player_account_id), sqlc.arg(refresh_hash),
    sqlc.arg(session_epoch), sqlc.arg(expires_at)
)
RETURNING id;

-- name: GetPlayerAccountSession :one
-- Session lookup joins the account so the caller can enforce epoch match and
-- the disabled gate in one round-trip.
SELECT
    s.id,
    s.player_account_id,
    s.session_epoch    AS snapshot_epoch,
    s.expires_at,
    s.revoked_at,
    a.email::text      AS email,
    a.display_name,
    a.disabled_at,
    a.session_epoch    AS account_epoch
FROM player_account_sessions s
JOIN player_accounts a ON a.id = s.player_account_id
WHERE s.refresh_hash = sqlc.arg(refresh_hash);

-- name: RevokePlayerAccountSession :exec
UPDATE player_account_sessions
SET revoked_at = now()
WHERE refresh_hash = sqlc.arg(refresh_hash)
  AND revoked_at IS NULL;

-- name: RevokeAllPlayerAccountSessions :exec
UPDATE player_account_sessions
SET revoked_at = now()
WHERE player_account_id = sqlc.arg(player_account_id)
  AND revoked_at IS NULL;

-- ListPlayerAccountLinkedProjects is intentionally NOT a sqlc query: it reads
-- the SECURITY DEFINER player_account_linked_projects(uuid) table-function,
-- which sqlc's analyzer can't resolve column types for. It is called via raw
-- tx.Query in internal/players (same approach as player_end_user_tenant).

-- name: LinkEndUserToAccount :exec
-- Tenant-scoped (run under Pool.Q with app.tenant_id set): attaches a
-- per-project end_user to a global account. Guarded by RLS on end_users.
UPDATE end_users
SET player_account_id = sqlc.arg(player_account_id)
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL;

-- name: UnlinkEndUserFromAccount :exec
UPDATE end_users
SET player_account_id = NULL
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL;

-- name: GetEndUserForAccountLink :one
-- Tenant-scoped: finds an existing (possibly unlinked) end_user by project +
-- email so a public-join / invite can link it instead of creating a duplicate.
SELECT id, player_account_id
FROM end_users
WHERE project_id = sqlc.arg(project_id)
  AND email = sqlc.arg(email)
  AND deleted_at IS NULL;

-- name: CreateLinkedEndUser :one
-- Tenant-scoped: creates a verified end_user already linked to a global
-- account (public-join / invite-accept). The account's email ownership is
-- already proven, so email_verified_at is set.
INSERT INTO end_users (
    tenant_id, project_id, external_id, email, email_verified_at, player_account_id
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(project_id),
    sqlc.arg(external_id),
    sqlc.arg(email),
    now(),
    sqlc.arg(player_account_id)
)
RETURNING id;

-- name: SearchPlayerAccounts :many
-- Platform-admin search by email prefix. Bounded LIMIT keeps the scan cheap.
SELECT
    id,
    email::text AS email,
    display_name,
    email_verified_at,
    disabled_at,
    created_at
FROM player_accounts
WHERE (sqlc.arg(query)::text = '' OR email ILIKE sqlc.arg(query)::text || '%')
ORDER BY created_at DESC
LIMIT sqlc.arg(row_limit)::int;
