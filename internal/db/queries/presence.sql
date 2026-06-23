-- name: UpsertPresence :exec
INSERT INTO presence (tenant_id, end_user_id, status, session_id, updated_at)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg('end_user_id'),
    sqlc.arg('status'),
    sqlc.arg('session_id'),
    now()
)
ON CONFLICT (tenant_id, end_user_id)
DO UPDATE SET
    status     = EXCLUDED.status,
    session_id = EXCLUDED.session_id,
    updated_at = now();

-- name: GetPresence :one
SELECT status, session_id, updated_at
FROM presence
WHERE tenant_id   = current_setting('app.tenant_id', true)::bigint
  AND end_user_id = sqlc.arg('end_user_id');

-- name: ListPresenceForUsers :many
SELECT end_user_id, status, session_id
FROM presence
WHERE tenant_id   = current_setting('app.tenant_id', true)::bigint
  AND end_user_id = ANY(sqlc.arg('end_user_ids')::bigint[]);
