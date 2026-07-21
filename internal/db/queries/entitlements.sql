-- name: SetTenantTierExact :execrows
-- Declarative entitlement apply: converge on an exact class in either
-- direction (downgrades rely on the existing grace path). 0 rows = no-op.
-- Runs in tenant RLS context.
UPDATE tenants
SET tier = sqlc.arg(tier)
WHERE id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
  AND tier <> sqlc.arg(tier);

-- name: DisableTenantFeatureGrant :execrows
-- Declarative entitlement apply: switch a tenant-level grant off in place so
-- the row survives (audit trail, later re-enable). Runs in tenant RLS context.
UPDATE feature_grants
SET enabled = false,
    reason = sqlc.narg(reason),
    updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id IS NULL
  AND feature = sqlc.arg(feature)
  AND enabled = true;
