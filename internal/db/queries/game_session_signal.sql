-- name: CountActiveSignalMembers :one
-- Counts how many of the given players are ACTIVE members (last_seen within
-- 30 s, matching ListGameSessionPeers/CountActiveGameSessionPeers) of an
-- open, unexpired session in this project. The signal handlers pass both
-- endpoints for a send (expecting 2) or just the caller for a poll
-- (expecting 1), so the count defines "both peers are in the same live
-- session" without diverging from the roster the clients themselves see.
SELECT count(DISTINCT p.player_id)
FROM game_session_peer p
JOIN game_session s ON s.id = p.session_id
WHERE s.id = sqlc.arg('session_id')
  AND s.project_id = sqlc.arg('project_id')
  AND s.state <> 'ended'
  AND s.expires_at > now()
  AND p.last_seen > now() - interval '30 seconds'
  AND p.player_id = ANY(sqlc.arg('player_ids')::bigint[]);

-- name: LockSignalRate :exec
-- Serializes a player's concurrent signal sends within the transaction so the
-- per-minute count+insert below can't be raced past the cap under READ
-- COMMITTED. player_id (project_players.id) is globally unique, so the lock
-- key never collides across tenants. Released on commit/rollback.
SELECT pg_advisory_xact_lock(hashtextextended('game_session_signal_rate', sqlc.arg('from_player_id')));

-- name: CountRecentGameSessionSignals :one
-- Signals this player has sent into this session in the trailing minute; the
-- handler rejects once it reaches the per-minute cap.
SELECT count(*) FROM game_session_signal
WHERE session_id = sqlc.arg('session_id')
  AND from_player_id = sqlc.arg('from_player_id')
  AND created_at > now() - interval '1 minute';

-- name: InsertGameSessionSignal :one
INSERT INTO game_session_signal
    (tenant_id, session_id, from_player_id, to_player_id, negotiation_id, kind, payload)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg('session_id'),
    sqlc.arg('from_player_id'),
    sqlc.arg('to_player_id'),
    sqlc.arg('negotiation_id'),
    sqlc.arg('kind'),
    sqlc.arg('payload')
)
RETURNING id;

-- name: DeleteExpiredGameSessionSignalsForTenant :execrows
-- Removes signals past their two-minute expiry for the current tenant. Called
-- per tenant by the game-session GC sweep so long-lived sessions don't
-- accumulate expired rows (session/tenant deletion already cascades the rest).
DELETE FROM game_session_signal
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND expires_at < now();

-- name: ListGameSessionSignalsForRecipient :many
-- Recipient-scoped, cursor-ordered, unexpired signals. The to_player_id filter
-- plus RLS ensure a player only ever reads signals addressed to them, and the
-- id > after_id cursor makes repeat polls return only new signals.
SELECT id, from_player_id, to_player_id, negotiation_id, kind, payload, created_at
FROM game_session_signal
WHERE session_id = sqlc.arg('session_id')
  AND to_player_id = sqlc.arg('to_player_id')
  AND id > sqlc.arg('after_id')
  AND expires_at > now()
ORDER BY id ASC
LIMIT 64;
