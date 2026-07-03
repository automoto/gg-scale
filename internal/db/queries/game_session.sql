-- name: CreateGameSession :one
INSERT INTO game_session (id, join_code, tenant_id, project_id, title_id, host_player_id, props, max_players, private, expires_at)
VALUES (
    sqlc.arg('id'),
    sqlc.arg('join_code'),
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg('project_id'),
    sqlc.arg('title_id'),
    sqlc.arg('host_player_id'),
    sqlc.arg('props'),
    sqlc.arg('max_players'),
    sqlc.arg('private'),
    sqlc.arg('expires_at')
)
RETURNING id, join_code, state, created_at;

-- name: GetGameSession :one
SELECT id, join_code, project_id, title_id, host_player_id, state, props, max_players, private, created_at, expires_at
FROM game_session
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg('id');

-- name: GetGameSessionForUpdate :one
-- Row-locking variant used by the join handler so concurrent joins for the
-- same session serialize on the session row and max_players is enforced
-- without a TOCTOU race.
SELECT id, join_code, project_id, title_id, host_player_id, state, props, max_players, private, created_at, expires_at
FROM game_session
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg('id')
FOR UPDATE;

-- name: GetGameSessionByJoinCode :one
-- Open, unexpired sessions only — an expired session lingering before GC
-- must not be resolvable by join code.
SELECT id, join_code, state
FROM game_session
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND join_code = sqlc.arg('join_code')
  AND state     = 'open'
  AND expires_at > now();

-- name: UpdateGameSessionState :exec
UPDATE game_session
SET state = sqlc.arg('state')
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg('id');

-- name: DeleteGameSession :exec
DELETE FROM game_session
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg('id');

-- name: LockProjectForGameSessionCreate :exec
-- Transaction-scoped advisory lock serializing session creation per project
-- so the open-session cap can't be raced past. Released on commit/rollback.
SELECT pg_advisory_xact_lock(hashtextextended('game_session_cap', sqlc.arg('project_id')));

-- name: CountOpenGameSessionsForProject :one
-- Counts non-ended, non-expired sessions for the project. Used in the
-- create handler to enforce a per-project session cap.
SELECT count(*) FROM game_session
WHERE project_id = sqlc.arg('project_id')
  AND tenant_id  = current_setting('app.tenant_id', true)::bigint
  AND state     != 'ended'
  AND expires_at > now();

-- name: DeleteExpiredGameSessionsForTenant :execrows
-- Removes sessions past their expiry for the current tenant. Called once
-- per tenant by the GC goroutine.
DELETE FROM game_session
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND expires_at < now();
