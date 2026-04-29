-- name: CreateAnonymousEndUser :one
INSERT INTO end_users (tenant_id, project_id, external_id)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1,
    $2
)
RETURNING id, external_id, created_at;

-- name: CreateEmailEndUser :one
INSERT INTO end_users (
    tenant_id, project_id, external_id, email, password_hash,
    email_verification_hash, email_verification_expires_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, $4, $5, $6
)
RETURNING id;

-- name: GetEndUserByEmail :one
SELECT id, project_id, password_hash, email_verified_at
FROM end_users
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND email = $2
  AND deleted_at IS NULL;

-- name: GetEndUserByExternalID :one
SELECT id, project_id, email_verified_at
FROM end_users
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND external_id = $2
  AND deleted_at IS NULL;

-- name: VerifyEmailByTokenHash :one
UPDATE end_users
SET email_verified_at              = now(),
    email_verification_hash        = NULL,
    email_verification_expires_at  = NULL
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND email_verification_hash = $1
  AND email_verification_expires_at > now()
RETURNING id;

-- name: CreateSession :one
INSERT INTO sessions (tenant_id, end_user_id, refresh_hash, expires_at)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3
)
RETURNING id, created_at;

-- name: GetSessionByRefreshHash :one
SELECT id, end_user_id, expires_at, revoked_at
FROM sessions
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND refresh_hash = $1;

-- name: RevokeSession :exec
UPDATE sessions
SET revoked_at = now()
WHERE id = $1
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND revoked_at IS NULL;

-- name: RevokeSessionByRefreshHash :exec
UPDATE sessions
SET revoked_at = now()
WHERE refresh_hash = $1
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND revoked_at IS NULL;

-- name: GetTenantCustomTokenSecret :one
SELECT custom_token_secret
FROM tenants
WHERE id = current_setting('app.tenant_id', true)::bigint;

-- name: UpsertEndUserByExternalID :one
-- Custom-token flow: find existing end_user with this external_id under
-- (tenant, project) or create one. Idempotent across repeated calls.
INSERT INTO end_users (tenant_id, project_id, external_id)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2
)
ON CONFLICT (tenant_id, project_id, external_id)
    WHERE deleted_at IS NULL
DO UPDATE SET external_id = EXCLUDED.external_id
RETURNING id;
