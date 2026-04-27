-- name: GetAPIKeyByHash :one
-- Bootstrap query used by the tenant middleware to resolve a Bearer token
-- to its tenant_id + project_id + tenant tier. Runs without an
-- app.tenant_id GUC set; the api_keys_bootstrap policy in 0010 lets it
-- through. Note: this query does NOT filter by tenants table RLS because
-- tenants.id = current_setting GUC is unset at bootstrap; if/when we add
-- a bootstrap policy on tenants, the JOIN keeps working.
SELECT k.id, k.tenant_id, k.project_id, k.revoked_at, t.tier
FROM api_keys k
JOIN tenants t ON t.id = k.tenant_id
WHERE k.key_hash = $1;

-- name: RevokeAPIKey :exec
UPDATE api_keys
SET revoked_at = now()
WHERE id = $1 AND tenant_id = current_setting('app.tenant_id', true)::bigint;

-- name: CreateAPIKey :one
INSERT INTO api_keys (tenant_id, project_id, key_hash, label, scopes)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, created_at;

-- name: ListAPIKeys :many
SELECT id, project_id, label, scopes, created_at, revoked_at
FROM api_keys
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
ORDER BY id DESC;
