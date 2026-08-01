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
