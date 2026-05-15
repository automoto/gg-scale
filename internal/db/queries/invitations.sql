-- Dashboard team invitations (operator-side: platform / tenant admins).

-- name: CreateDashboardInvitation :one
INSERT INTO dashboard_invitations (
    email, tenant_id, role, code_hash, expires_at, invited_by_user_id
)
VALUES (
    sqlc.arg(email),
    sqlc.narg(tenant_id)::bigint,
    sqlc.arg(role),
    sqlc.arg(code_hash),
    sqlc.arg(expires_at),
    sqlc.arg(invited_by_user_id)
)
RETURNING id, created_at, expires_at;

-- name: GetDashboardInvitationByCodeHash :one
SELECT
    i.id,
    i.email::text AS email,
    i.tenant_id,
    i.role,
    i.expires_at,
    i.accepted_at,
    i.revoked_at,
    i.invited_by_user_id,
    i.created_at,
    t.name AS tenant_name
FROM dashboard_invitations i
LEFT JOIN tenants t ON t.id = i.tenant_id
WHERE i.code_hash = sqlc.arg(code_hash)
  AND i.accepted_at IS NULL
  AND i.revoked_at IS NULL;

-- name: GetDashboardInvitationByID :one
SELECT
    id,
    email::text AS email,
    tenant_id,
    role,
    expires_at,
    accepted_at,
    revoked_at,
    invited_by_user_id,
    created_at
FROM dashboard_invitations
WHERE id = sqlc.arg(id);

-- name: ListDashboardInvitationsForTenant :many
SELECT
    id,
    email::text AS email,
    role,
    expires_at,
    invited_by_user_id,
    created_at
FROM dashboard_invitations
WHERE tenant_id = sqlc.arg(tenant_id)
  AND accepted_at IS NULL
  AND revoked_at IS NULL
ORDER BY created_at DESC;

-- name: ListPlatformAdminInvitations :many
SELECT
    id,
    email::text AS email,
    role,
    expires_at,
    invited_by_user_id,
    created_at
FROM dashboard_invitations
WHERE tenant_id IS NULL
  AND accepted_at IS NULL
  AND revoked_at IS NULL
ORDER BY created_at DESC;

-- name: MarkDashboardInvitationAccepted :exec
UPDATE dashboard_invitations
SET accepted_at = now()
WHERE id = sqlc.arg(id)
  AND accepted_at IS NULL
  AND revoked_at IS NULL;

-- name: RevokeDashboardInvitation :exec
UPDATE dashboard_invitations
SET revoked_at = now()
WHERE id = sqlc.arg(id)
  AND accepted_at IS NULL
  AND revoked_at IS NULL;

-- name: ListDashboardMembersForTenant :many
SELECT
    m.id AS membership_id,
    u.id AS user_id,
    u.email::text AS email,
    m.role,
    u.is_platform_admin,
    u.last_login_at,
    m.created_at
FROM dashboard_memberships m
JOIN dashboard_users u ON u.id = m.dashboard_user_id
WHERE m.tenant_id = sqlc.arg(tenant_id)
ORDER BY m.created_at ASC;

-- name: ListPlatformAdmins :many
SELECT
    id AS user_id,
    email::text AS email,
    last_login_at,
    created_at
FROM dashboard_users
WHERE is_platform_admin = true
ORDER BY created_at ASC;

-- name: DeleteDashboardMembership :exec
DELETE FROM dashboard_memberships
WHERE id = sqlc.arg(id)
  AND tenant_id = sqlc.arg(tenant_id);

-- name: CreateDashboardMembership :one
INSERT INTO dashboard_memberships (dashboard_user_id, tenant_id, role)
VALUES (sqlc.arg(dashboard_user_id), sqlc.arg(tenant_id), sqlc.arg(role))
ON CONFLICT (dashboard_user_id, tenant_id) DO UPDATE
    SET role = EXCLUDED.role
RETURNING id;

-- name: PromoteDashboardUserToPlatformAdmin :exec
UPDATE dashboard_users
SET is_platform_admin = true
WHERE id = sqlc.arg(id);

-- End-user (player) invitations.

-- name: CreateEndUserInvitation :one
INSERT INTO end_user_invitations (
    tenant_id, project_id, email, code_hash, expires_at, invited_by_user_id
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(project_id),
    sqlc.arg(email),
    sqlc.arg(code_hash),
    sqlc.arg(expires_at),
    sqlc.arg(invited_by_user_id)
)
RETURNING id, created_at, expires_at;

-- name: GetEndUserInvitationByCodeHash :one
SELECT
    i.id,
    i.tenant_id,
    i.project_id,
    i.email::text AS email,
    i.expires_at,
    i.accepted_at,
    i.revoked_at,
    i.invited_by_user_id,
    i.created_at,
    p.name AS project_name
FROM end_user_invitations i
JOIN projects p ON p.id = i.project_id
WHERE i.code_hash = sqlc.arg(code_hash)
  AND i.accepted_at IS NULL
  AND i.revoked_at IS NULL;

-- name: ListEndUserInvitationsForProject :many
SELECT
    id,
    email::text AS email,
    expires_at,
    invited_by_user_id,
    created_at
FROM end_user_invitations
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND accepted_at IS NULL
  AND revoked_at IS NULL
ORDER BY created_at DESC;

-- name: MarkEndUserInvitationAccepted :exec
UPDATE end_user_invitations
SET accepted_at = now()
WHERE id = sqlc.arg(id)
  AND accepted_at IS NULL
  AND revoked_at IS NULL;

-- name: RevokeEndUserInvitation :exec
UPDATE end_user_invitations
SET revoked_at = now()
WHERE id = sqlc.arg(id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND accepted_at IS NULL
  AND revoked_at IS NULL;

-- Project → tenant lookup (privileged; used by the player UI which knows
-- the project from the URL but has no tenant context yet).

-- name: GetProjectTenant :one
SELECT id, tenant_id, name
FROM projects
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL;

-- name: GetEndUserByEmailProject :one
-- Privileged variant of GetEndUserByEmail used by the player UI before
-- the tenant context is set; the caller already knows the project_id from
-- the URL and looks up tenant + verification state in one shot.
SELECT
    id,
    tenant_id,
    project_id,
    password_hash,
    email_verified_at,
    email_verification_code_hash,
    email_verification_salt,
    email_verification_expires_at,
    email_verification_attempts,
    email_verification_last_sent_at,
    disabled_at
FROM end_users
WHERE project_id = sqlc.arg(project_id)
  AND email = sqlc.arg(email)
  AND deleted_at IS NULL;

-- name: CreatePlayerEndUser :one
-- Used by the player UI signup flow; takes project_id explicitly because
-- the player site doesn't have an api_key bearer.
INSERT INTO end_users (
    tenant_id, project_id, external_id, email, password_hash,
    email_verification_code_hash, email_verification_salt,
    email_verification_expires_at, email_verification_last_sent_at
)
SELECT
    p.tenant_id,
    p.id,
    sqlc.arg(external_id),
    sqlc.arg(email),
    sqlc.arg(password_hash),
    sqlc.arg(code_hash),
    sqlc.arg(code_salt),
    sqlc.arg(expires_at),
    now()
FROM projects p
WHERE p.id = sqlc.arg(project_id)
  AND p.deleted_at IS NULL
RETURNING id, tenant_id;

-- name: MarkPlayerVerified :exec
UPDATE end_users
SET email_verified_at               = now(),
    email_verification_code_hash    = NULL,
    email_verification_salt         = NULL,
    email_verification_expires_at   = NULL,
    email_verification_attempts     = 0
WHERE id = sqlc.arg(id);

-- name: IncrementPlayerVerificationAttempts :one
UPDATE end_users
SET email_verification_attempts = email_verification_attempts + 1
WHERE id = sqlc.arg(id)
RETURNING email_verification_attempts;

-- name: SetPlayerVerificationCode :exec
UPDATE end_users
SET email_verification_code_hash    = sqlc.arg(code_hash),
    email_verification_salt         = sqlc.arg(code_salt),
    email_verification_expires_at   = sqlc.arg(expires_at),
    email_verification_attempts     = 0,
    email_verification_last_sent_at = now()
WHERE id = sqlc.arg(id);

-- name: GetPlayerVerificationStateByID :one
SELECT
    email_verified_at,
    email_verification_code_hash,
    email_verification_salt,
    email_verification_expires_at,
    email_verification_attempts,
    email_verification_last_sent_at
FROM end_users
WHERE id = sqlc.arg(id);

-- name: CreatePlayerSession :one
INSERT INTO sessions (tenant_id, end_user_id, refresh_hash, expires_at)
SELECT u.tenant_id, sqlc.arg(end_user_id), sqlc.arg(refresh_hash), sqlc.arg(expires_at)
FROM end_users u
WHERE u.id = sqlc.arg(end_user_id)
RETURNING id;

-- name: GetPlayerSession :one
SELECT s.id, s.end_user_id, s.expires_at, s.revoked_at, u.email, u.project_id, u.tenant_id, u.disabled_at
FROM sessions s
JOIN end_users u ON u.id = s.end_user_id
WHERE s.refresh_hash = sqlc.arg(refresh_hash);

-- name: RevokePlayerSession :exec
UPDATE sessions
SET revoked_at = now()
WHERE refresh_hash = sqlc.arg(refresh_hash) AND revoked_at IS NULL;

-- name: SetPlayerDisabled :exec
UPDATE end_users
SET disabled_at = $2
WHERE id = $1;

-- name: PlayerInviteLookup :one
-- Privileged (SECURITY DEFINER) lookup used by the player invite-accept
-- page. Returns the tenant_id so the caller can SET app.tenant_id and
-- continue under normal RLS enforcement.
SELECT id, tenant_id, project_id, email::text AS email, expires_at, project_name
FROM player_invite_lookup(sqlc.arg(code_hash));
