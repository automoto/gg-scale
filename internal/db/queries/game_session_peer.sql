-- name: UpsertGameSessionPeer :exec
INSERT INTO game_session_peer (tenant_id, session_id, player_id, ip, port, qos, last_seen)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg('session_id'),
    sqlc.arg('player_id'),
    sqlc.arg('ip'),
    sqlc.arg('port'),
    sqlc.arg('qos'),
    now()
)
ON CONFLICT (session_id, player_id)
DO UPDATE SET
    ip        = EXCLUDED.ip,
    port      = EXCLUDED.port,
    qos       = EXCLUDED.qos,
    last_seen = now();

-- name: TouchGameSessionPeer :execrows
-- Returns rows affected (0 when the caller isn't a member of the session)
-- so the heartbeat handler can reject non-members instead of leaking the
-- roster.
UPDATE game_session_peer
SET last_seen = now(),
    qos       = CASE WHEN sqlc.arg('qos')::jsonb IS NOT NULL THEN sqlc.arg('qos') ELSE qos END
WHERE session_id  = sqlc.arg('session_id')
  AND player_id = sqlc.arg('player_id');

-- name: ListGameSessionPeers :many
-- Returns active peers (last_seen within 30 s) with each peer's optional
-- xuid. RLS on game_session_peer scopes rows to the current tenant.
SELECT
    p.player_id,
    p.ip,
    p.port,
    p.qos,
    p.last_seen,
    u.xuid
FROM game_session_peer p
LEFT JOIN project_players u ON u.id = p.player_id
WHERE p.session_id = sqlc.arg('session_id')
  AND p.last_seen  > now() - interval '30 seconds'
ORDER BY p.last_seen ASC;

-- name: IsGameSessionMember :one
SELECT EXISTS (
    SELECT 1 FROM game_session_peer
    WHERE session_id  = sqlc.arg('session_id')
      AND player_id = sqlc.arg('player_id')
) AS is_member;

-- name: CountActiveGameSessionPeers :one
-- Counts peers seen within the activity window, excluding a given user so a
-- re-joining member doesn't count against the session's capacity.
SELECT count(*) FROM game_session_peer
WHERE session_id   = sqlc.arg('session_id')
  AND player_id != sqlc.arg('exclude_user_id')
  AND last_seen    > now() - interval '30 seconds';

-- name: DeleteGameSessionPeer :exec
DELETE FROM game_session_peer
WHERE session_id  = sqlc.arg('session_id')
  AND player_id = sqlc.arg('player_id');

-- name: DeleteAllGameSessionPeers :exec
-- Used when the host ends the session so peer rows don't linger until GC.
DELETE FROM game_session_peer
WHERE session_id = sqlc.arg('session_id');

-- name: PruneStaleGameSessionPeers :execrows
DELETE FROM game_session_peer
WHERE session_id = sqlc.arg('session_id')
  AND last_seen  < now() - interval '30 seconds';
