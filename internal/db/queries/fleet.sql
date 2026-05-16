-- name: CreatePendingAllocation :one
INSERT INTO game_server_allocations (
    tenant_id, project_id, fleet_id, backend, region, status, metadata
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, $4, 'pending', $5
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
SELECT id, tenant_id, project_id, fleet_id, backend, backend_ref, region, address,
       status::text AS status, metadata, requested_at, ready_at, released_at
FROM game_server_allocations
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: ListActiveAllocations :many
SELECT id, tenant_id, project_id, fleet_id, backend, backend_ref, region, address,
       status::text AS status, metadata, requested_at, ready_at, released_at
FROM game_server_allocations
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND released_at IS NULL
ORDER BY id DESC
LIMIT $2;

-- name: ListAllocationsForProject :many
-- Dashboard fleet list: optionally include terminal rows (shutdown/failed)
-- and paginate. include_terminal=false keeps the page focused on live
-- allocations; the UI toggles it via a query param.
SELECT id, tenant_id, project_id, fleet_id, backend, backend_ref, region, address,
       status::text AS status, metadata, requested_at, ready_at, released_at
FROM game_server_allocations
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND (sqlc.arg(include_terminal)::boolean OR released_at IS NULL)
ORDER BY id DESC
LIMIT sqlc.arg(lim)
OFFSET sqlc.arg(off);

-- name: CountAllocationsForProject :one
SELECT count(*)::bigint
FROM game_server_allocations
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND (sqlc.arg(include_terminal)::boolean OR released_at IS NULL);

-- name: ListAllocationBackendsForTenant :many
-- Distinct backends seen across this tenant's recent allocations. Drives
-- the backends-health page so operators see which backends actually serve
-- traffic, not just the one currently configured.
SELECT backend, count(*)::bigint AS allocation_count
FROM game_server_allocations
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
GROUP BY backend
ORDER BY backend;

-- name: InsertAllocationEvent :exec
-- Append an event for the watch stream. The trim trigger keeps history
-- bounded per allocation_id; callers can fire-and-forget.
INSERT INTO fleet_allocation_events (
    tenant_id, allocation_id, status, address, err_message
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(allocation_id),
    sqlc.arg(status)::allocation_status,
    sqlc.arg(address),
    sqlc.arg(err_message)
);

-- name: ListAllocationEvents :many
SELECT id, allocation_id, status::text AS status, address, err_message, created_at
FROM fleet_allocation_events
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND allocation_id = sqlc.arg(allocation_id)
ORDER BY id DESC
LIMIT sqlc.arg(lim);
