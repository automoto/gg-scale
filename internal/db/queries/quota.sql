-- name: GetTenantQuotaContext :one
-- The current tenant's class and quota-enforcement flag. Read inside an
-- RLS-scoped tx (app.tenant_id set) before a quota-gated growth operation.
SELECT tier, enforce_quotas
FROM tenants
WHERE id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
FOR UPDATE;

-- name: CountProjectsForTenant :one
-- Live (non-soft-deleted) project count for the current tenant.
SELECT count(*)::bigint
FROM projects
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: CountPlayersForTenant :one
-- Registered (non-soft-deleted) player count for the current tenant, across
-- all its projects.
SELECT count(*)::bigint
FROM project_players
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: SetTenantEnforceQuotas :exec
-- Flip the per-tenant enforcement flag. Used by provisioning when the operator
-- has enabled quota enforcement for new tenants. Runs in a bootstrap tx.
UPDATE tenants
SET enforce_quotas = sqlc.arg(enforce_quotas)
WHERE id = sqlc.arg(tenant_id);
