-- name: LatestSettingsRevision :one
SELECT COALESCE(MAX(revision), 0)::bigint
FROM settings_revisions
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND resource_kind = sqlc.arg(resource_kind)
  AND resource_id = sqlc.arg(resource_id);

-- name: InsertSettingsRevision :exec
INSERT INTO settings_revisions (
    tenant_id, project_id, resource_kind, resource_id, revision, snapshot,
    actor_user_id, mcp_token_id, source
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(project_id), sqlc.arg(resource_kind), sqlc.arg(resource_id),
    sqlc.arg(revision), sqlc.arg(snapshot), sqlc.narg(actor_user_id),
    sqlc.narg(mcp_token_id), sqlc.arg(source)
);

-- name: PruneSettingsRevisions :exec
DELETE FROM settings_revisions
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND resource_kind = sqlc.arg(resource_kind)
  AND resource_id = sqlc.arg(resource_id)
  AND revision <= sqlc.arg(below_or_at);

-- name: ListSettingsRevisions :many
SELECT revision, snapshot, actor_user_id, mcp_token_id, source, created_at
FROM settings_revisions
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND resource_kind = sqlc.arg(resource_kind)
  AND resource_id = sqlc.arg(resource_id)
ORDER BY revision DESC;

-- name: GetSettingsRevision :one
SELECT snapshot
FROM settings_revisions
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND resource_kind = sqlc.arg(resource_kind)
  AND resource_id = sqlc.arg(resource_id)
  AND revision = sqlc.arg(revision);
