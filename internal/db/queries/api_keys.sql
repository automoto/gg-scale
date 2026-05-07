-- name: GetAPIKeyByHash :one
-- Bootstrap query used by the tenant middleware to resolve a Bearer token
-- to its tenant_id + project_id + tenant tier + key_type. Runs without an
-- app.tenant_id GUC set; the api_keys_bootstrap policy in 0010 lets it
-- through. Note: this query does NOT filter by tenants table RLS because
-- tenants.id = current_setting GUC is unset at bootstrap; if/when we add
-- a bootstrap policy on tenants, the JOIN keeps working.
SELECT k.id, k.tenant_id, k.project_id, k.key_type, k.revoked_at, t.tier
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

-- name: CreateDashboardAPIKey :one
WITH args AS (
    SELECT
        sqlc.narg(project_id)::bigint AS project_id,
        sqlc.arg(key_hash)::bytea AS key_hash,
        sqlc.arg(label)::text AS label
), tenant_ctx AS (
    SELECT nullif(current_setting('app.tenant_id', true), '')::bigint AS tenant_id
),
project_ctx AS (
    SELECT args.project_id
    FROM args
    WHERE args.project_id IS NULL
    UNION ALL
    SELECT p.id AS project_id
    FROM projects p, tenant_ctx t, args
    WHERE p.id = args.project_id AND p.tenant_id = t.tenant_id
)
INSERT INTO api_keys (tenant_id, project_id, key_hash, label, scopes)
SELECT t.tenant_id, p.project_id, args.key_hash, nullif(trim(args.label), ''), '{}'::text[]
FROM tenant_ctx t
CROSS JOIN project_ctx p
CROSS JOIN args
RETURNING id, created_at;

-- name: ListAPIKeys :many
SELECT k.id, k.project_id, p.name AS project_name, k.label, k.scopes, k.created_at, k.revoked_at
FROM api_keys k
LEFT JOIN projects p ON p.id = k.project_id
WHERE k.tenant_id = current_setting('app.tenant_id', true)::bigint
ORDER BY k.id DESC;

-- name: UpdateAPIKeyLabel :exec
UPDATE api_keys
SET label = nullif(trim(sqlc.arg(label)::text), '')
WHERE id = sqlc.arg(id) AND tenant_id = current_setting('app.tenant_id', true)::bigint;
