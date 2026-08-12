-- name: GetConnectionLimitOverride :one
-- Tenant-level realtime admission envelope. The WebSocket admission path falls
-- back to compiled tier defaults when no row exists.
SELECT sustained, ceiling
FROM connection_limit_overrides
WHERE tenant_id = $1;

-- name: UpsertConnectionLimitOverride :exec
INSERT INTO connection_limit_overrides (tenant_id, sustained, ceiling, updated_by, updated_at)
VALUES ($1, $2, $3, sqlc.narg(updated_by), now())
ON CONFLICT (tenant_id)
DO UPDATE SET sustained = EXCLUDED.sustained,
              ceiling = EXCLUDED.ceiling,
              updated_by = EXCLUDED.updated_by,
              updated_at = now();

-- name: DeleteConnectionLimitOverride :exec
DELETE FROM connection_limit_overrides
WHERE tenant_id = $1;
