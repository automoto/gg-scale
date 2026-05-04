-- name: ListProjectsForTenant :many
SELECT id, name, created_at
FROM projects
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
ORDER BY name;

-- name: CreateProjectForTenant :one
INSERT INTO projects (tenant_id, name)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    trim(sqlc.arg(name)::text)
)
RETURNING id, name, created_at;

-- name: DashboardCreateTenant :one
SELECT
    r.tenant_id::bigint AS tenant_id,
    r.project_id::bigint AS project_id,
    r.api_key_id::bigint AS api_key_id,
    r.membership_id::bigint AS membership_id
FROM dashboard_create_tenant(
    sqlc.arg(actor_user_id),
    sqlc.arg(tenant_name),
    sqlc.arg(project_name),
    sqlc.arg(key_hash),
    sqlc.arg(key_label)
) AS r(tenant_id, project_id, api_key_id, membership_id);
