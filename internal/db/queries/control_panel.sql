-- name: ListProjectsForTenant :many
SELECT id, name, created_at, public_joining_enabled
FROM projects
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
ORDER BY name;

-- name: GetTenantFacts :one
SELECT name, tier, enforce_quotas, public_joining_enabled, disabled_at, disabled_by
FROM tenants
WHERE id = $1
  AND deleted_at IS NULL;

-- name: GetTenantDisabledState :one
-- Lean per-request probe used by the control panel's tenant-access gate:
-- a platform-disabled tenant locks its tenant admins out.
SELECT disabled_at, disabled_by
FROM tenants
WHERE id = $1
  AND deleted_at IS NULL;

-- name: DisableTenantBySelf :execrows
-- Tenant self-disable; 0 rows means already disabled or gone.
UPDATE tenants
SET disabled_at = now(),
    disabled_by = 'tenant'
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL
  AND disabled_at IS NULL;

-- name: DisableTenantByPlatformAdmin :execrows
-- A platform disable supersedes an existing self-disable: it promotes
-- disabled_by to 'platform' in place, with no re-enable window. The original
-- disabled_at is kept. 0 rows only when already platform-disabled or gone.
UPDATE tenants
SET disabled_at = COALESCE(disabled_at, now()),
    disabled_by = 'platform'
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL
  AND (disabled_at IS NULL OR disabled_by = 'tenant');

-- name: EnableTenantByTenantAdmin :execrows
-- A tenant admin can only undo a self-disable; 0 rows on a platform disable.
UPDATE tenants
SET disabled_at = NULL,
    disabled_by = NULL
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL
  AND disabled_by = 'tenant';

-- name: EnableTenantByPlatformAdmin :execrows
UPDATE tenants
SET disabled_at = NULL,
    disabled_by = NULL
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL
  AND disabled_at IS NOT NULL;

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
