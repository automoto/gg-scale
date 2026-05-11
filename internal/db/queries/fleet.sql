-- name: CreatePendingAllocation :one
INSERT INTO game_server_allocations (
    tenant_id, project_id, backend, region, status, metadata
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, 'pending', $4
)
RETURNING id, requested_at;

-- name: MarkAllocationReady :exec
UPDATE game_server_allocations
SET status      = 'ready',
    backend_ref = $2,
    address     = $3,
    ready_at    = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: SetAllocationStatus :exec
UPDATE game_server_allocations
SET status = $2
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: MarkAllocationFailed :exec
UPDATE game_server_allocations
SET status      = 'failed',
    released_at = COALESCE(released_at, now())
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: ReleaseAllocation :exec
UPDATE game_server_allocations
SET status      = 'shutdown',
    released_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: GetAllocation :one
SELECT id, tenant_id, project_id, backend, backend_ref, region, address,
       status::text AS status, metadata, requested_at, ready_at, released_at
FROM game_server_allocations
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: ListActiveAllocations :many
SELECT id, tenant_id, project_id, backend, backend_ref, region, address,
       status::text AS status, metadata, requested_at, ready_at, released_at
FROM game_server_allocations
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND released_at IS NULL
ORDER BY id DESC
LIMIT $2;
