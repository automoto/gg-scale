-- name: GetAllowedOrigins :one
SELECT allowed_origins
FROM projects
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: GetAllowedOriginsForUpdate :one
SELECT allowed_origins
FROM projects
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
FOR UPDATE;

-- name: SetAllowedOrigins :exec
UPDATE projects
SET allowed_origins = sqlc.arg(origins)::text[]
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: ListAllProjectAllowedOrigins :many
-- Runs with no tenant set; see all_project_allowed_origins().
SELECT origin::text FROM all_project_allowed_origins() AS origin;
