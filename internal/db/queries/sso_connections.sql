-- Single sign-on connections. Both tables are platform-global (no tenant
-- RLS): every query here runs through db.Pool.BootstrapQ. The subject always
-- comes from a verified provider round trip, never from client input.

-- name: GetPlayerAccountByConnection :one
SELECT
    a.id,
    a.email::text AS email,
    a.display_name,
    a.disabled_at,
    a.session_epoch
FROM player_account_connections c
JOIN player_accounts a ON a.id = c.player_account_id
WHERE c.provider = sqlc.arg(provider)
  AND c.subject = sqlc.arg(subject);

-- name: ListPlayerAccountConnections :many
SELECT provider, created_at
FROM player_account_connections
WHERE player_account_id = sqlc.arg(player_account_id)
ORDER BY provider;

-- name: InsertPlayerAccountConnection :exec
INSERT INTO player_account_connections (player_account_id, provider, subject)
VALUES (sqlc.arg(player_account_id), sqlc.arg(provider), sqlc.arg(subject));

-- name: TouchPlayerAccountConnection :exec
UPDATE player_account_connections
SET last_used_at = now()
WHERE provider = sqlc.arg(provider)
  AND subject = sqlc.arg(subject);

-- name: LockPlayerAccountSignInMethods :one
-- Row lock for the last-sign-in-method rule: two concurrent unlink requests
-- serialize here, so they cannot both pass the count and remove the last
-- two methods.
SELECT (password_hash IS NOT NULL)::boolean AS has_password
FROM player_accounts
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: CountPlayerAccountConnections :one
SELECT count(*)::bigint
FROM player_account_connections
WHERE player_account_id = sqlc.arg(player_account_id);

-- name: DeletePlayerAccountConnection :execrows
DELETE FROM player_account_connections
WHERE player_account_id = sqlc.arg(player_account_id)
  AND provider = sqlc.arg(provider);

-- name: GetControlPanelUserByConnection :one
-- Status-blind like GetControlPanelUserAnyStatusByEmail: the caller refuses a
-- disabled user with the same answer as an unknown connection.
SELECT
    u.id,
    u.email::text AS email,
    u.is_platform_admin,
    u.email_verified_at,
    u.disabled_at
FROM control_panel_user_connections c
JOIN control_panel_users u ON u.id = c.control_panel_user_id
WHERE c.provider = sqlc.arg(provider)
  AND c.subject = sqlc.arg(subject);

-- name: ListControlPanelUserConnections :many
SELECT provider, created_at
FROM control_panel_user_connections
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id)
ORDER BY provider;

-- name: InsertControlPanelUserConnection :exec
INSERT INTO control_panel_user_connections (control_panel_user_id, provider, subject)
VALUES (sqlc.arg(control_panel_user_id), sqlc.arg(provider), sqlc.arg(subject));

-- name: TouchControlPanelUserConnection :exec
UPDATE control_panel_user_connections
SET last_used_at = now()
WHERE provider = sqlc.arg(provider)
  AND subject = sqlc.arg(subject);

-- name: LockControlPanelUserSignInMethods :one
-- Same row lock as LockPlayerAccountSignInMethods.
SELECT (password_hash IS NOT NULL)::boolean AS has_password
FROM control_panel_users
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: CountControlPanelUserConnections :one
SELECT count(*)::bigint
FROM control_panel_user_connections
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id);

-- name: DeleteControlPanelUserConnection :execrows
DELETE FROM control_panel_user_connections
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id)
  AND provider = sqlc.arg(provider);
