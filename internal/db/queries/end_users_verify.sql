-- name: GetEndUserForVerify :one
-- Lookup an end-user by ID for the POST /v1/end-users/verify endpoint
-- (gameserver session-token verification). Filters deleted/disabled
-- accounts so a forged token claiming a stale id rejects with 401.
-- The explicit tenant_id predicate matches the project's RLS-via-GUC
-- convention (Pool.Q sets app.tenant_id) and survives a hypothetical
-- RLS disable; the JOINs against tenants/projects enforce the
-- soft-delete kill-switch — a wound-down project can't continue to
-- verify sessions.
SELECT u.id,
       u.tenant_id,
       u.project_id,
       u.external_id,
       coalesce(u.email, '')::text AS email
FROM end_users u
JOIN tenants  t ON t.id = u.tenant_id  AND t.deleted_at IS NULL
JOIN projects p ON p.id = u.project_id AND p.deleted_at IS NULL
WHERE u.id = $1
  AND u.tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint
  AND u.deleted_at IS NULL
  AND u.disabled_at IS NULL;
