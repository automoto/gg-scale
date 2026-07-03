-- name: GetAPIRateLimitOverride :one
-- Tenant-level HTTP API override (project_id NULL, kind 'api'). Read by the
-- rate-limit middleware; falls back to compiled tier defaults when absent.
SELECT rate, burst
FROM rate_limit_overrides
WHERE tenant_id = $1 AND project_id IS NULL AND kind = 'api';

-- name: GetInviteRateLimitOverride :one
-- Most-specific-wins: a project-scoped row overrides a tenant-wide row for the
-- same kind. Used by the invite throttle.
SELECT rate, burst
FROM rate_limit_overrides
WHERE tenant_id = $1
  AND kind = $2
  AND (project_id IS NULL OR project_id = $3)
ORDER BY (project_id IS NULL)
LIMIT 1;

-- name: ListAllRateLimitOverridesForTenant :many
-- Every override for a tenant — tenant-wide (project_id NULL) and per-project —
-- in one query. The rate-limits page groups these in Go rather than issuing one
-- ListRateLimitOverridesForProject per project (an N+1 over the project list).
SELECT id, project_id, kind, rate, burst, updated_by, updated_at
FROM rate_limit_overrides
WHERE tenant_id = $1
ORDER BY project_id NULLS FIRST, kind;

-- name: GetTenantTier :one
-- The tenant's billing tier, used to show the correct compiled default on the
-- rate-limits page (enforcement keys off the same tier via the API key).
SELECT tier FROM tenants WHERE id = $1;

-- name: UpsertRateLimitOverride :exec
INSERT INTO rate_limit_overrides (tenant_id, project_id, kind, rate, burst, updated_by, updated_at)
VALUES ($1, sqlc.narg(project_id), $2, $3, $4, sqlc.narg(updated_by), now())
ON CONFLICT (tenant_id, COALESCE(project_id, 0), kind)
DO UPDATE SET rate = EXCLUDED.rate, burst = EXCLUDED.burst, updated_by = EXCLUDED.updated_by, updated_at = now();

-- name: DeleteRateLimitOverride :exec
DELETE FROM rate_limit_overrides
WHERE tenant_id = $1 AND kind = $2
  AND ((project_id IS NULL AND sqlc.narg(project_id)::bigint IS NULL) OR project_id = sqlc.narg(project_id));
