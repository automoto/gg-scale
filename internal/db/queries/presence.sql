-- name: UpsertPresence :exec
INSERT INTO presence (tenant_id, player_id, status, session_id, updated_at)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg('player_id'),
    sqlc.arg('status'),
    sqlc.arg('session_id'),
    now()
)
ON CONFLICT (tenant_id, player_id)
DO UPDATE SET
    status     = EXCLUDED.status,
    session_id = EXCLUDED.session_id,
    updated_at = now();

-- name: GetPresence :one
SELECT status, session_id, updated_at
FROM presence
WHERE tenant_id   = current_setting('app.tenant_id', true)::bigint
  AND player_id = sqlc.arg('player_id');

-- name: ListPresenceForUsers :many
SELECT player_id, status, session_id
FROM presence
WHERE tenant_id   = current_setting('app.tenant_id', true)::bigint
  AND player_id = ANY(sqlc.arg('player_ids')::bigint[]);
