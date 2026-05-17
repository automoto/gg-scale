-- Dashboard-side player management queries. Privileged: the dashboard
-- runs them as platform/tenant admin via BootstrapQ, so we filter by
-- tenant_id explicitly rather than relying on RLS.

-- name: ListPlayersForProject :many
SELECT
    u.id,
    u.external_id,
    coalesce(u.email::text, '') AS email,
    u.email_verified_at,
    u.disabled_at,
    u.created_at
FROM end_users u
JOIN projects p ON p.id = u.project_id
WHERE p.tenant_id = sqlc.arg(tenant_id)
  AND u.project_id = sqlc.arg(project_id)
  AND u.deleted_at IS NULL
  AND (sqlc.narg(email_filter)::text IS NULL OR coalesce(u.email::text, '') ILIKE '%' || sqlc.narg(email_filter)::text || '%')
ORDER BY u.created_at DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountPlayersForProject :one
SELECT COUNT(*)::bigint
FROM end_users u
JOIN projects p ON p.id = u.project_id
WHERE p.tenant_id = sqlc.arg(tenant_id)
  AND u.project_id = sqlc.arg(project_id)
  AND u.deleted_at IS NULL
  AND (sqlc.narg(email_filter)::text IS NULL OR coalesce(u.email::text, '') ILIKE '%' || sqlc.narg(email_filter)::text || '%');

-- name: GetPlayerForProject :one
SELECT
    u.id,
    u.external_id,
    coalesce(u.email::text, '') AS email,
    u.email_verified_at,
    u.disabled_at,
    u.created_at,
    u.tenant_id,
    u.project_id
FROM end_users u
JOIN projects p ON p.id = u.project_id
WHERE p.tenant_id = sqlc.arg(tenant_id)
  AND u.project_id = sqlc.arg(project_id)
  AND u.id = sqlc.arg(id)
  AND u.deleted_at IS NULL;

-- name: SetPlayerDisabledByTenant :exec
UPDATE end_users
SET disabled_at = sqlc.arg(disabled_at)
WHERE id = sqlc.arg(id)
  AND project_id = sqlc.arg(project_id)
  AND tenant_id = sqlc.arg(tenant_id)
  AND deleted_at IS NULL;
