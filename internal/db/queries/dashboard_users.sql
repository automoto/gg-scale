-- name: CountDashboardUsers :one
SELECT count(*)::bigint FROM dashboard_users;

-- name: CreateDashboardUser :one
INSERT INTO dashboard_users (email, password_hash, is_platform_admin)
VALUES (sqlc.arg(email), sqlc.arg(password_hash), sqlc.arg(is_platform_admin))
RETURNING id, email::text AS email, is_platform_admin, created_at;

-- name: CreateFirstDashboardAdmin :one
INSERT INTO dashboard_users (email, password_hash, is_platform_admin)
SELECT sqlc.arg(email), sqlc.arg(password_hash), true
WHERE NOT EXISTS (SELECT 1 FROM dashboard_users)
RETURNING id, email::text AS email, is_platform_admin, created_at;

-- name: GetDashboardUserByEmail :one
-- Disabled accounts (disabled_at IS NOT NULL) are filtered out here so
-- /v1/dashboard/login behaves identically to an unknown email — same
-- dummy bcrypt + invalid_credentials response.
SELECT
    id,
    email::text AS email,
    password_hash,
    is_platform_admin,
    login_failures,
    locked_until,
    last_login_at,
    email_verified_at,
    created_at
FROM dashboard_users
WHERE email = sqlc.arg(email)
  AND disabled_at IS NULL;

-- name: GetDashboardUserAnyStatusByEmail :one
-- Status-blind variant used ONLY by the invite-accept code path so we
-- can distinguish "no row" (truly new email — create user) from "row
-- exists but disabled" (refuse with errInviteForDisabledAccount).
-- DO NOT use this for authentication.
SELECT
    id,
    email::text AS email,
    password_hash,
    is_platform_admin,
    disabled_at
FROM dashboard_users
WHERE email = sqlc.arg(email);

-- name: GetDashboardUserVerificationState :one
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
FROM dashboard_users
WHERE id = sqlc.arg(id);

-- name: LockDashboardUserVerification :exec
-- Set the lockout window on an account that just tipped over
-- MaxLifetimeAttempts. The Go side computes the timestamp so the lockout
-- duration stays a single source of truth.
UPDATE dashboard_users
SET email_verification_locked_until = sqlc.arg(locked_until)::timestamptz
WHERE id = sqlc.arg(id);

-- name: SetDashboardUserVerificationCode :exec
UPDATE dashboard_users
SET email_verification_code_hash    = sqlc.arg(code_hash),
    email_verification_salt         = sqlc.arg(code_salt),
    email_verification_expires_at   = sqlc.arg(expires_at),
    email_verification_attempts     = 0,
    email_verification_last_sent_at = now()
WHERE id = sqlc.arg(id);

-- name: IncrementDashboardVerificationAttempts :one
UPDATE dashboard_users
SET email_verification_attempts = email_verification_attempts + 1
WHERE id = sqlc.arg(id)
RETURNING email_verification_attempts;

-- name: ReserveDashboardVerifyAttempt :one
-- Atomic check-and-bump used in place of the previous fetch-then-increment
-- pattern: two parallel wrong codes used to both pass the cap check before
-- either incremented, so the lockout could be overshot. The WHERE clause
-- now folds the bound into the same statement that mutates the counter.
-- Returns 0 rows when already at cap (caller treats as errVerifyLocked).
UPDATE dashboard_users
SET email_verification_attempts = email_verification_attempts + 1,
    email_verification_lifetime_attempts = email_verification_lifetime_attempts + 1
WHERE id = sqlc.arg(id)
  AND email_verification_attempts < sqlc.arg(max_attempts)::int
RETURNING email_verification_attempts, email_verification_lifetime_attempts;

-- name: ClearDashboardVerificationCode :exec
UPDATE dashboard_users
SET email_verification_code_hash    = NULL,
    email_verification_salt         = NULL,
    email_verification_expires_at   = NULL,
    email_verification_attempts     = 0
WHERE id = sqlc.arg(id);

-- name: MarkDashboardUserVerified :exec
UPDATE dashboard_users
SET email_verified_at               = now(),
    email_verification_code_hash    = NULL,
    email_verification_salt         = NULL,
    email_verification_expires_at   = NULL,
    email_verification_attempts     = 0
WHERE id = sqlc.arg(id);

-- name: GetDashboardUserByID :one
SELECT
    id,
    email::text AS email,
    is_platform_admin,
    created_at
FROM dashboard_users
WHERE id = sqlc.arg(id);

-- name: RecordDashboardLoginSuccess :exec
UPDATE dashboard_users
SET login_failures = 0,
    locked_until = NULL,
    last_login_at = now()
WHERE id = sqlc.arg(id);

-- name: BumpDashboardLoginFailure :one
-- Atomic increment + conditional lockout. The previous read-then-write
-- pattern was TOCTOU-racy: N parallel failed logins all read the same value
-- and wrote N+1, so 10 simultaneous wrong passwords landed at login_failures=1
-- and the lockout never fired. UPDATE...RETURNING serialises under the row
-- lock pgx already takes.
UPDATE dashboard_users
SET login_failures = login_failures + 1,
    locked_until = CASE
        WHEN login_failures + 1 >= sqlc.arg(failure_limit)::int
            THEN sqlc.arg(lockout_until)::timestamptz
        ELSE locked_until
    END
WHERE id = sqlc.arg(id)
RETURNING login_failures, locked_until;

-- name: UpdateDashboardPassword :exec
UPDATE dashboard_users
SET password_hash = sqlc.arg(password_hash),
    login_failures = 0,
    locked_until = NULL
WHERE id = sqlc.arg(id);

-- name: RevokeAllDashboardSessionsForUser :exec
UPDATE dashboard_sessions
SET revoked_at = now()
WHERE dashboard_user_id = sqlc.arg(dashboard_user_id)
  AND revoked_at IS NULL;

-- name: CreateDashboardSession :one
INSERT INTO dashboard_sessions (
    dashboard_user_id, refresh_hash, csrf_secret, expires_at, ip, user_agent
)
VALUES (
    sqlc.arg(dashboard_user_id),
    sqlc.arg(refresh_hash),
    sqlc.arg(csrf_secret),
    sqlc.arg(expires_at),
    sqlc.narg(ip),
    sqlc.narg(user_agent)
)
RETURNING id, expires_at, created_at;

-- name: GetDashboardSessionByRefreshHash :one
-- Joined to dashboard_users so the session dies as soon as a platform
-- admin sets disabled_at, without needing a separate session-purge step
-- on every request path. requireSession maps ErrNoRows to a redirect to
-- /login, which is what we want for disabled accounts.
SELECT
    s.id,
    s.dashboard_user_id,
    s.csrf_secret,
    s.expires_at,
    s.revoked_at,
    s.created_at,
    u.email::text AS email,
    u.is_platform_admin
FROM dashboard_sessions s
JOIN dashboard_users u ON u.id = s.dashboard_user_id
WHERE s.refresh_hash = sqlc.arg(refresh_hash)
  AND u.disabled_at IS NULL;

-- name: TouchDashboardSession :exec
UPDATE dashboard_sessions
SET last_seen_at = now(),
    expires_at = LEAST(sqlc.arg(expires_at)::timestamptz, created_at + interval '7 days')
WHERE id = sqlc.arg(id)
  AND revoked_at IS NULL;

-- name: RevokeDashboardSession :exec
UPDATE dashboard_sessions
SET revoked_at = now()
WHERE id = sqlc.arg(id)
  AND revoked_at IS NULL;

-- name: ListDashboardTenantsForUser :many
SELECT
    t.id,
    t.name,
    m.role,
    t.created_at
FROM dashboard_memberships m
JOIN tenants t ON t.id = m.tenant_id
WHERE m.dashboard_user_id = sqlc.arg(dashboard_user_id)
  AND t.deleted_at IS NULL
ORDER BY t.id DESC;

-- name: ListDashboardTenantsForPlatformAdmin :many
SELECT
    t.id,
    t.name,
    'owner'::text AS role,
    t.created_at
FROM tenants t
WHERE t.deleted_at IS NULL
ORDER BY t.id DESC;

-- name: GetDashboardMembership :one
SELECT id, role
FROM dashboard_memberships
WHERE dashboard_user_id = sqlc.arg(dashboard_user_id)
  AND tenant_id = sqlc.arg(tenant_id);

-- name: CreateVerifiedDashboardUser :one
-- Used by invite acceptance: creates a new user who is verified by
-- definition (they had to click the invite link in their inbox).
INSERT INTO dashboard_users (
    email, password_hash, is_platform_admin, email_verified_at
)
VALUES (
    sqlc.arg(email),
    sqlc.arg(password_hash),
    sqlc.arg(is_platform_admin),
    now()
)
RETURNING id, email::text AS email, is_platform_admin, created_at;

-- name: ListDashboardUsersForPlatformAdmin :many
-- Powers the /v1/dashboard/admin/users page. tenant_count is a
-- correlated subquery so users with zero memberships still appear.
SELECT
    u.id,
    u.email::text AS email,
    u.is_platform_admin,
    u.disabled_at,
    u.last_login_at,
    u.created_at,
    (SELECT COUNT(*)::bigint FROM dashboard_memberships m WHERE m.dashboard_user_id = u.id) AS tenant_count
FROM dashboard_users u
WHERE (sqlc.narg(email_filter)::text IS NULL OR u.email::text ILIKE '%' || sqlc.narg(email_filter)::text || '%')
ORDER BY u.created_at DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountDashboardUsersForPlatformAdmin :one
SELECT COUNT(*)::bigint
FROM dashboard_users u
WHERE (sqlc.narg(email_filter)::text IS NULL OR u.email::text ILIKE '%' || sqlc.narg(email_filter)::text || '%');

-- name: CountEnabledPlatformAdmins :one
SELECT COUNT(*)::bigint
FROM dashboard_users
WHERE is_platform_admin = true
  AND disabled_at IS NULL;

-- name: LockEnabledPlatformAdmins :many
-- Serializes last-admin checks by locking the currently enabled platform
-- admin rows before counting them in the surrounding transaction.
SELECT id
FROM dashboard_users
WHERE is_platform_admin = true
  AND disabled_at IS NULL
ORDER BY id
FOR UPDATE;

-- name: SetDashboardUserDisabled :exec
-- Nullable timestamptz so the same query handles disable (now()) and
-- enable (NULL).
UPDATE dashboard_users
SET disabled_at = sqlc.narg(disabled_at)::timestamptz
WHERE id = sqlc.arg(id);

-- name: RevokeOpenInvitationsByInviter :exec
-- Bulk-revoke the outgoing invitations a (now-disabled) user created.
-- Re-enabling does NOT un-revoke these; the platform admin can re-issue.
UPDATE dashboard_invitations
SET revoked_at = now()
WHERE invited_by_user_id = sqlc.arg(invited_by_user_id)
  AND accepted_at IS NULL
  AND revoked_at IS NULL;
