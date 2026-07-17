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

-- name: SoftDeleteStorageObject :one
-- Soft-deletes and returns the freed byte count (octet_length(value::text)) so
-- the caller can decrement the tenant storage counter in the same tx. Returns
-- no rows when the object is already absent (delete is idempotent).
UPDATE storage_objects
SET deleted_at = now(),
    updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = $1
  AND owner_user_id = $2
  AND key = $3
  AND deleted_at IS NULL
RETURNING octet_length(value::text)::bigint AS freed_bytes;

-- name: LockStorageObjectForWrite :exec
-- Transaction-scoped advisory lock serializing concurrent writes (put/delete)
-- to a single storage object so the read-modify-write of the tenant byte
-- counter can't be raced past. Released on commit/rollback.
SELECT pg_advisory_xact_lock(hashtextextended(
    current_setting('app.tenant_id', true) || ':' ||
    sqlc.arg('project_id')::bigint::text || ':' ||
    sqlc.arg('owner_user_id')::bigint::text || ':' ||
    sqlc.arg('key')::text, 0));

-- name: StorageUsageForWrite :one
-- One-shot inputs for the pre-write storage-quota check: the tenant's current
-- metered total, the existing object's size (0 if new), and the incoming
-- value's size — all measured as octet_length(value::text) so they match the
-- counter maintained by ApplyTenantStorageDelta. Runs in the write tx
-- (app.tenant_id set), serialized per object by LockStorageObjectForWrite.
SELECT
    COALESCE((SELECT total_bytes FROM tenant_storage_usage
              WHERE tenant_id = current_setting('app.tenant_id', true)::bigint), 0)::bigint AS total_bytes,
    COALESCE((SELECT octet_length(value::text) FROM storage_objects
              WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
                AND project_id = $1 AND owner_user_id = $2 AND key = $3
                AND deleted_at IS NULL), 0)::bigint AS old_bytes,
    octet_length(sqlc.arg(new_value)::jsonb::text)::bigint AS new_bytes;

-- name: ApplyTenantStorageDelta :exec
-- Adjust the current tenant's storage counter by delta (bytes; negative on
-- delete/shrink), clamped at zero. Called in the write tx after a successful
-- object write so the counter stays in lockstep. Runs in tenant context.
INSERT INTO tenant_storage_usage (tenant_id, total_bytes, updated_at)
VALUES (current_setting('app.tenant_id', true)::bigint, GREATEST(0, sqlc.arg(delta)::bigint), now())
ON CONFLICT (tenant_id) DO UPDATE
    SET total_bytes = GREATEST(0, tenant_storage_usage.total_bytes + sqlc.arg(delta)::bigint),
        updated_at = now();

-- name: GetTenantStorageUsageByID :one
-- Metered storage total for a tenant by id (0 if never written). Read in a
-- bootstrap tx by the control-panel settings page for the used/limit display.
SELECT COALESCE((SELECT total_bytes FROM tenant_storage_usage
                 WHERE tenant_id = sqlc.arg(tenant_id)), 0)::bigint;

-- name: ListEnforcedTenantStorage :many
-- Name, usage, class, and last-notified threshold for every quota-enforced
-- tenant. Read cross-tenant by the storage-warn River job (bootstrap tx).
SELECT u.tenant_id, t.name, t.tier, u.total_bytes, u.last_notified_threshold
FROM tenant_storage_usage u
JOIN tenants t ON t.id = u.tenant_id
WHERE t.enforce_quotas = true
  AND t.deleted_at IS NULL;

-- name: SetTenantStorageNotifiedThreshold :exec
UPDATE tenant_storage_usage
SET last_notified_threshold = sqlc.arg(threshold)
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: ListTenantAdminEmails :many
-- Verified emails of a tenant's owner/admin members, for operational notices
-- (e.g. storage-quota warnings). Read cross-tenant by background jobs.
SELECT u.email
FROM control_panel_memberships m
JOIN control_panel_users u ON u.id = m.control_panel_user_id
WHERE m.tenant_id = sqlc.arg(tenant_id)
  AND m.role IN ('owner', 'admin')
  AND u.email_verified_at IS NOT NULL
ORDER BY u.email;

-- name: ListStorageObjects :many
SELECT id, key, octet_length(value::text)::bigint AS size_bytes, version, updated_at
FROM storage_objects
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND owner_user_id = sqlc.arg(owner_user_id)
  AND deleted_at IS NULL
  AND key LIKE CAST(sqlc.arg(key_prefix) AS text) || '%' ESCAPE '\'
  AND id > sqlc.arg(cursor_id)
ORDER BY id ASC
LIMIT sqlc.arg(row_limit);
