-- Fleet templates: operator-defined recipes that allocations are drawn from.
-- Lookup is by project-scoped name (the public identifier used by the SDK,
-- matchmaker API, and control panel URLs). Soft delete keeps historical
-- allocations referenceable in the UI.

-- name: CreateFleet :one
INSERT INTO fleets (
    tenant_id, project_id, name, backend, config
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(project_id),
    sqlc.arg(name),
    sqlc.arg(backend),
    sqlc.arg(config)
)
RETURNING id, tenant_id, project_id, name, backend, config, created_at, updated_at, deleted_at;

-- name: GetFleetByID :one
SELECT id, tenant_id, project_id, name, backend, config, created_at, updated_at, deleted_at
FROM fleets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: GetFleetByName :one
-- Resolves a project-scoped name to a fleet row. Soft-deleted rows are
-- excluded so a retired fleet doesn't collide with a freshly created one
-- under the same name.
SELECT id, tenant_id, project_id, name, backend, config, created_at, updated_at, deleted_at
FROM fleets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND name = sqlc.arg(name)
  AND deleted_at IS NULL;

-- name: ListFleetsForProject :many
-- Control panel list. Soft-deleted rows are excluded; include_deleted is reserved
-- for a future "archive" view but not wired through the UI yet.
SELECT id, tenant_id, project_id, name, backend, config, created_at, updated_at, deleted_at
FROM fleets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND deleted_at IS NULL
ORDER BY name;

-- name: ListDeletedFleetsForProject :many
-- Deleted fleets of a project, newest deletion first, for the restore list.
SELECT id, tenant_id, project_id, name, backend, config, created_at, updated_at, deleted_at
FROM fleets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND deleted_at IS NOT NULL
ORDER BY deleted_at DESC, id DESC;

-- name: GetProjectFleet :one
-- A live or deleted fleet of one project. A row of another project is not found.
SELECT id, tenant_id, project_id, name, backend, config, created_at, updated_at, deleted_at
FROM fleets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id);

-- name: UpdateProjectFleet :one
UPDATE fleets
SET name       = sqlc.arg(name),
    config     = sqlc.arg(config),
    updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND deleted_at IS NULL
RETURNING id, tenant_id, project_id, name, backend, config, created_at, updated_at, deleted_at;

-- name: SoftDeleteProjectFleet :one
UPDATE fleets
SET deleted_at = now(),
    updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND deleted_at IS NULL
RETURNING id, tenant_id, project_id, name, backend, config, created_at, updated_at, deleted_at;

-- name: RestoreProjectFleet :one
-- Clears deleted_at. It fails with a unique violation when a live fleet of the
-- project has the same name.
UPDATE fleets
SET deleted_at = NULL,
    updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND deleted_at IS NOT NULL
RETURNING id, tenant_id, project_id, name, backend, config, created_at, updated_at, deleted_at;
