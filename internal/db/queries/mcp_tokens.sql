-- name: GetMCPTokenByHash :one
-- Runs with no tenant set (mcp_tokens_bootstrap and tenants_bootstrap allow
-- it). projects has no bootstrap policy, so the project check is a second
-- query in the tenant scope.
SELECT k.id, k.tenant_id, k.project_id, k.created_by_user_id, k.label, k.scopes,
       k.expires_at, k.revoked_at, k.last_used_at,
       (t.disabled_at IS NOT NULL OR t.deleted_at IS NOT NULL)::bool AS tenant_disabled,
       (u.disabled_at IS NOT NULL)::bool AS creator_disabled
FROM mcp_tokens k
JOIN tenants t ON t.id = k.tenant_id
JOIN control_panel_users u ON u.id = k.created_by_user_id
WHERE k.token_hash = sqlc.arg(token_hash);

-- name: ProjectIsLive :one
SELECT EXISTS (
    SELECT 1 FROM projects
    WHERE id = sqlc.arg(project_id)
      AND tenant_id = current_setting('app.tenant_id', true)::bigint
      AND deleted_at IS NULL
)::bool;

-- name: TouchMCPTokenLastUsed :exec
-- At most one write per minute for each token.
UPDATE mcp_tokens
SET last_used_at = now()
WHERE id = sqlc.arg(id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: LockLiveProject :one
-- Serializes creates that check a per-project limit.
SELECT id FROM projects
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
FOR UPDATE;

-- name: CountActiveMCPTokens :one
SELECT count(*)::bigint
FROM mcp_tokens
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND revoked_at IS NULL
  AND expires_at > now();

-- name: CreateMCPToken :one
INSERT INTO mcp_tokens (
    tenant_id, project_id, created_by_user_id, label, token_hash, token_hint,
    scopes, expires_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(project_id), sqlc.arg(created_by_user_id), sqlc.arg(label),
    sqlc.arg(token_hash), sqlc.arg(token_hint), sqlc.arg(scopes)::text[],
    sqlc.arg(expires_at)
)
RETURNING id;

-- name: ListMCPTokensForProject :many
SELECT k.id, k.label, k.token_hint, k.scopes, k.created_by_user_id,
       u.email AS creator_email, k.expires_at, k.last_used_at, k.created_at
FROM mcp_tokens k
JOIN control_panel_users u ON u.id = k.created_by_user_id
WHERE k.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND k.project_id = sqlc.arg(project_id)
  AND k.revoked_at IS NULL
ORDER BY k.id DESC
LIMIT 100;

-- name: GetMCPTokenCreator :one
SELECT created_by_user_id
FROM mcp_tokens
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND revoked_at IS NULL;

-- name: RevokeMCPToken :execrows
UPDATE mcp_tokens
SET revoked_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND id = sqlc.arg(id)
  AND revoked_at IS NULL;
