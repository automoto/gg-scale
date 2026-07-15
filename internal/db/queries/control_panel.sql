-- name: ListProjectsForTenant :many
SELECT id, name, created_at, public_joining_enabled
FROM projects
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
ORDER BY name;

-- name: GetTenantFacts :one
SELECT name, tier, enforce_quotas, public_joining_enabled
FROM tenants
WHERE id = $1
  AND deleted_at IS NULL;

-- name: SetTenantTierByID :one
-- Platform-admin direct tier changes may move in either direction. Capture the
-- prior tier under the same row lock so the audit record cannot race another
-- administrator's update.
WITH current_tenant AS MATERIALIZED (
    SELECT tenants.id, tenants.tier AS old_tier
    FROM tenants
    WHERE tenants.id = sqlc.arg(tenant_id)
      AND tenants.deleted_at IS NULL
    FOR UPDATE
), updated AS (
    UPDATE tenants AS t
    SET tier = sqlc.arg(tier)
    FROM current_tenant AS current
    WHERE t.id = current.id
    RETURNING current.old_tier, t.tier AS new_tier
)
SELECT old_tier, new_tier FROM updated;

-- name: SetTenantPublicJoining :exec
UPDATE tenants
SET public_joining_enabled = sqlc.arg(enabled)
WHERE id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: SetProjectPublicJoining :exec
UPDATE projects
SET public_joining_enabled = sqlc.arg(enabled)
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: CreateProjectForTenant :one
INSERT INTO projects (tenant_id, name)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    trim(sqlc.arg(name)::text)
)
RETURNING id, name, created_at;

-- name: ControlPanelCreateTenant :one
SELECT
    r.tenant_id::bigint AS tenant_id,
    r.project_id::bigint AS project_id,
    r.api_key_id::bigint AS api_key_id,
    r.membership_id::bigint AS membership_id
FROM control_panel_create_tenant(
    sqlc.arg(actor_user_id),
    sqlc.arg(tenant_name),
    sqlc.arg(project_name),
    sqlc.arg(key_hash),
    sqlc.arg(key_label)
) AS r(tenant_id, project_id, api_key_id, membership_id);
