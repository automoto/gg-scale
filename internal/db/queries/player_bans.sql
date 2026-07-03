-- Tenant-wide player bans. tenant_player_bans has no RLS, so every query
-- filters tenant_id explicitly. Enforcement runs in both tenant Pool.Q and
-- account BootstrapQ contexts.

-- name: CreateTenantPlayerBan :exec
INSERT INTO tenant_player_bans (tenant_id, player_account_id, reason, created_by)
VALUES (sqlc.arg(tenant_id), sqlc.arg(player_account_id), sqlc.narg(reason), sqlc.narg(created_by))
ON CONFLICT (tenant_id, player_account_id)
DO UPDATE SET reason = sqlc.narg(reason), created_by = sqlc.narg(created_by), created_at = now();

-- name: DeleteTenantPlayerBan :execrows
DELETE FROM tenant_player_bans
WHERE tenant_id = sqlc.arg(tenant_id)
  AND player_account_id = sqlc.arg(player_account_id);

-- name: IsAccountBannedInTenant :one
SELECT id FROM tenant_player_bans
WHERE tenant_id = sqlc.arg(tenant_id)
  AND player_account_id = sqlc.arg(player_account_id)
LIMIT 1;

-- name: IsPlayerBannedByTenant :one
-- Enforcement helper: is the given player's linked account tenant-banned in
-- the player's own tenant? Runs in a tenant Pool.Q (project_players RLS-filtered).
-- Returns pgx.ErrNoRows when not banned (or the player is unlinked).
SELECT b.id
FROM project_players u
JOIN tenant_player_bans b
  ON b.player_account_id = u.player_account_id
 AND b.tenant_id = u.tenant_id
WHERE u.id = sqlc.arg(player_id)
  AND u.deleted_at IS NULL
LIMIT 1;

-- name: ListTenantPlayerBans :many
-- Dashboard list for a tenant, enriched with the banned account's email.
SELECT b.player_account_id, a.email::text AS email, b.reason, b.created_at
FROM tenant_player_bans b
JOIN player_accounts a ON a.id = b.player_account_id
WHERE b.tenant_id = sqlc.arg(tenant_id)
ORDER BY b.created_at DESC;

-- name: BumpPlayerSessionEpoch :exec
-- Single player (project disable path).
UPDATE project_players
SET session_epoch = session_epoch + 1
WHERE id = sqlc.arg(id);

-- name: BumpAccountPlayerEpochsInTenant :exec
-- Bump every player of an account within a tenant (tenant-ban path), so all
-- their live JWTs are rejected at server-verify immediately.
UPDATE project_players
SET session_epoch = session_epoch + 1
WHERE tenant_id = sqlc.arg(tenant_id)
  AND player_account_id = sqlc.arg(player_account_id);
