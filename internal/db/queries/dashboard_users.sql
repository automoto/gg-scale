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
SELECT
    id,
    email::text AS email,
    password_hash,
    is_platform_admin,
    login_failures,
    locked_until,
    last_login_at,
    created_at
FROM dashboard_users
WHERE email = sqlc.arg(email);

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

-- name: RecordDashboardLoginFailure :one
UPDATE dashboard_users
SET login_failures = sqlc.arg(login_failures),
    locked_until = sqlc.narg(locked_until)::timestamptz
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
WHERE s.refresh_hash = sqlc.arg(refresh_hash);

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
