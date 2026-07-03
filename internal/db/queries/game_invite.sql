-- name: GetPlayerIDByEmail :one
-- Resolves an invite recipient by email within the caller's project.
SELECT id FROM project_players
WHERE tenant_id  = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg('project_id')
  AND email      = sqlc.arg('email')
  AND deleted_at IS NULL;

-- name: CreateGameInvite :one
INSERT INTO game_invite (tenant_id, project_id, from_player_id, to_player_id, session_id, join_code, expires_at)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg('project_id'),
    sqlc.arg('from_player_id'),
    sqlc.arg('to_player_id'),
    sqlc.arg('session_id'),
    sqlc.arg('join_code'),
    sqlc.arg('expires_at')
)
RETURNING id;

-- name: DeleteGameInvite :execrows
-- Either sender (cancel) or recipient (decline/dismiss) may delete an invite.
DELETE FROM game_invite
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id        = sqlc.arg('id')
  AND (from_player_id = sqlc.arg('caller_id') OR to_player_id = sqlc.arg('caller_id'));

-- name: ListPendingGameInvites :many
-- Returns unexpired invites for the target user, enriched with the sender's
-- email and optional xuid for display.
SELECT
    gi.id,
    gi.from_player_id,
    gi.session_id,
    gi.join_code,
    gi.expires_at,
    COALESCE(u.email::text, '')::text AS from_email,
    u.xuid                            AS from_xuid
FROM game_invite gi
LEFT JOIN project_players u ON u.id = gi.from_player_id
WHERE gi.tenant_id   = current_setting('app.tenant_id', true)::bigint
  AND gi.to_player_id  = sqlc.arg('to_player_id')
  AND gi.expires_at  > now()
ORDER BY gi.id ASC;

-- name: DeleteExpiredGameInvitesForTenant :execrows
-- Removes invites past their expiry for the current tenant. Called per
-- tenant by the GC goroutine.
DELETE FROM game_invite
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND expires_at < now();
