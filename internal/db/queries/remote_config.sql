-- name: GetRemoteConfig :one
SELECT remote_config
FROM projects
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: GetRemoteConfigForControlPanel :one
SELECT remote_config
FROM projects
WHERE id = sqlc.arg(project_id)
  AND tenant_id = sqlc.arg(tenant_id)
  AND deleted_at IS NULL;

-- name: UpdateRemoteConfig :execrows
UPDATE projects
SET remote_config = sqlc.arg(remote_config)
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;
