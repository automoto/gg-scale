-- Public player lookup. Project-scoped: an id from a sibling project resolves
-- to no rows (the handler 404s). Only public fields are selected — never the
-- account email.

-- name: GetPublicPlayer :one
SELECT p.id, a.display_name, p.created_at
FROM project_players p
LEFT JOIN player_accounts a ON a.id = p.player_account_id
WHERE p.id = sqlc.arg(id)
  AND p.project_id = sqlc.arg(project_id)
  AND p.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND p.deleted_at IS NULL;

-- name: GetPublicPlayerByFriendCode :one
-- Friend-code resolve: same public shape and project scoping as GetPublicPlayer.
SELECT p.id, a.display_name, p.created_at
FROM project_players p
LEFT JOIN player_accounts a ON a.id = p.player_account_id
WHERE p.friend_code = sqlc.arg(friend_code)
  AND p.project_id = sqlc.arg(project_id)
  AND p.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND p.deleted_at IS NULL;

-- name: SetPlayerFriendCodeIfAbsent :execrows
-- Lazy first-read initialization: 0 rows means a concurrent reader won the
-- race (re-read) or the caller already has a code.
UPDATE project_players
SET friend_code = sqlc.arg(friend_code)
WHERE id = sqlc.arg(id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
  AND friend_code IS NULL;

-- name: SetPlayerFriendCode :execrows
-- Regenerate: overwrites unconditionally, invalidating the old code.
-- 0 rows = soft-deleted or missing caller.
UPDATE project_players
SET friend_code = sqlc.arg(friend_code)
WHERE id = sqlc.arg(id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: GetPlayerModerationState :one
-- Server-tier gate: does the named player exist in the caller's project, and
-- may a game server act for them? The tenants/projects JOINs mirror
-- GetPlayerForVerify so a wound-down project can't keep taking writes.
SELECT p.id, p.disabled_at
FROM project_players p
JOIN tenants  t ON t.id = p.tenant_id  AND t.deleted_at IS NULL
JOIN projects j ON j.id = p.project_id AND j.deleted_at IS NULL
WHERE p.id = sqlc.arg(id)
  AND p.project_id = sqlc.arg(project_id)
  AND p.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND p.deleted_at IS NULL;

-- name: ListPublicPlayers :many
-- Unknown and out-of-project ids drop out of the result set silently.
SELECT p.id, a.display_name, p.created_at
FROM project_players p
LEFT JOIN player_accounts a ON a.id = p.player_account_id
WHERE p.id = ANY(sqlc.arg('ids')::bigint[])
  AND p.project_id = sqlc.arg(project_id)
  AND p.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND p.deleted_at IS NULL
ORDER BY p.id;
