-- Friend edges between GLOBAL player_accounts. friend_edges has no tenant_id
-- and no RLS, so these run in either a tenant Pool.Q or a BootstrapQ
-- transaction. Account ids are UUIDs.

-- name: RequestFriendByAccount :one
-- One current row per directed pair. Re-requests after rejection update in
-- place; pending/accepted are idempotent (WHERE filters them, DO UPDATE
-- no-ops); blocked is terminal (WHERE omits it).
INSERT INTO friend_edges (from_account_id, to_account_id, status)
VALUES (sqlc.arg(from_account_id), sqlc.arg(to_account_id), 'pending')
ON CONFLICT (from_account_id, to_account_id)
DO UPDATE SET status = 'pending', updated_at = now()
WHERE friend_edges.status = 'rejected'
RETURNING id, status;

-- name: GetFriendEdgeByAccount :one
SELECT id, status
FROM friend_edges
WHERE from_account_id = sqlc.arg(from_account_id)
  AND to_account_id   = sqlc.arg(to_account_id);

-- name: SetFriendEdgeStatusByAccount :exec
UPDATE friend_edges
SET status = sqlc.arg(status), updated_at = now()
WHERE from_account_id = sqlc.arg(from_account_id)
  AND to_account_id   = sqlc.arg(to_account_id);

-- name: UpsertFriendEdgeStatusByAccount :exec
-- Used for block: create-or-overwrite the directed edge to a terminal status
-- regardless of the prior state (blocking a stranger, a pending, or a friend).
INSERT INTO friend_edges (from_account_id, to_account_id, status)
VALUES (sqlc.arg(from_account_id), sqlc.arg(to_account_id), sqlc.arg(status))
ON CONFLICT (from_account_id, to_account_id)
DO UPDATE SET status = sqlc.arg(status), updated_at = now();

-- name: DeleteFriendEdgeByAccount :execrows
-- Symmetric unfriend: caller can be on either side. Never removes a 'blocked'
-- edge — a block is cleared only via the directed unblock path
-- (DeleteFriendEdgeDirected), so a blockee cannot delete the blocker's block.
DELETE FROM friend_edges
WHERE ((from_account_id = sqlc.arg('me') AND to_account_id = sqlc.arg('other'))
    OR (from_account_id = sqlc.arg('other') AND to_account_id = sqlc.arg('me')))
  AND status <> 'blocked';

-- name: DeleteFriendEdgeDirected :execrows
-- Directed delete (unblock: only remove the edge the caller initiated).
DELETE FROM friend_edges
WHERE from_account_id = sqlc.arg(from_account_id)
  AND to_account_id   = sqlc.arg(to_account_id)
  AND status = sqlc.arg(status);

-- name: ListFriendsByStatusForAccount :many
-- Caller-aware. For 'blocked', only rows the caller initiated are returned —
-- never rows where the caller is the blockee (which would let a blocked user
-- learn who blocked them).
SELECT id, from_account_id, to_account_id, status, created_at, updated_at
FROM friend_edges
WHERE ((sqlc.arg('status')::text != 'blocked'
            AND (from_account_id = sqlc.arg('me') OR to_account_id = sqlc.arg('me')))
       OR (sqlc.arg('status')::text = 'blocked'
            AND from_account_id = sqlc.arg('me')))
  AND status = sqlc.arg('status')
  AND id > sqlc.arg('cursor')
ORDER BY id ASC
LIMIT sqlc.arg('row_limit');

-- name: AreAccountsFriendsAccepted :one
-- Edge id if an accepted friendship exists in either direction.
SELECT id FROM friend_edges
WHERE ((from_account_id = sqlc.arg('a') AND to_account_id = sqlc.arg('b'))
       OR (from_account_id = sqlc.arg('b') AND to_account_id = sqlc.arg('a')))
  AND status = 'accepted'
LIMIT 1;

-- name: IsBlockedBetweenAccounts :one
-- Edge id if EITHER account has blocked the other. Defense-in-depth gate on
-- every interaction path (friend request, game invite, presence).
SELECT id FROM friend_edges
WHERE ((from_account_id = sqlc.arg('a') AND to_account_id = sqlc.arg('b'))
       OR (from_account_id = sqlc.arg('b') AND to_account_id = sqlc.arg('a')))
  AND status = 'blocked'
LIMIT 1;

-- name: ListAccountIdentities :many
-- Bulk-fetch email + display_name for a set of accounts (friend-list enrich).
SELECT id, email::text AS email, display_name
FROM player_accounts
WHERE id = ANY(sqlc.arg('account_ids')::uuid[]);

-- name: FindAccountIDByEmail :one
SELECT id FROM player_accounts WHERE email = sqlc.arg(email);

-- name: FindAccountIDsByDisplayName :many
-- Exact display-name match. LIMIT 2 lets the caller detect ambiguity (display
-- names are not unique) and refuse rather than friend the wrong person.
SELECT id, email::text AS email
FROM player_accounts
WHERE display_name = sqlc.arg(display_name)
LIMIT 2;

-- name: GetPlayerLinkedAccountID :one
-- Tenant-scoped: the global account a player is linked to (NULL if the
-- player is anonymous / unlinked).
SELECT player_account_id
FROM project_players
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL;

-- name: ResolvePlayersForAccountsInProject :many
-- Tenant-scoped: maps a set of accounts back to their player in a specific
-- project, for presence sharing and JSON-API user_id mapping.
SELECT id AS player_id, player_account_id
FROM project_players
WHERE project_id = sqlc.arg(project_id)
  AND player_account_id = ANY(sqlc.arg('account_ids')::uuid[])
  AND deleted_at IS NULL;
