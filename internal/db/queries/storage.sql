-- name: PutStorageObject :one
-- Upsert; bumps version. Caller may pass If-Match via expected_version param.
INSERT INTO storage_objects (
    tenant_id, project_id, owner_user_id, key, value, version, updated_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, $4, 1, now()
)
ON CONFLICT (tenant_id, project_id, owner_user_id, key)
    WHERE deleted_at IS NULL
DO UPDATE SET value      = EXCLUDED.value,
              version    = storage_objects.version + 1,
              updated_at = now()
RETURNING id, version, updated_at;

-- name: PutStorageObjectIfMatch :one
-- Optimistic concurrency variant — only updates if the row's current
-- version matches expected. RETURNING NULL row on mismatch.
UPDATE storage_objects
SET value      = $5,
    version    = version + 1,
    updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND owner_user_id = $2
  AND key = $3
  AND version = $4
  AND deleted_at IS NULL
RETURNING id, version, updated_at;

-- name: GetStorageObject :one
SELECT id, value, version, updated_at
FROM storage_objects
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND owner_user_id = $2
  AND key = $3
  AND deleted_at IS NULL;

-- name: SoftDeleteStorageObject :exec
UPDATE storage_objects
SET deleted_at = now(),
    updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND owner_user_id = $2
  AND key = $3
  AND deleted_at IS NULL;

-- name: ListStorageObjects :many
SELECT id, key, value, version, updated_at
FROM storage_objects
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND owner_user_id = $2
  AND deleted_at IS NULL
  AND ($3::text = '' OR key LIKE $3 || '%')
  AND id > $4
ORDER BY id ASC
LIMIT $5;
