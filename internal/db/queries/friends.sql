-- name: RequestFriend :one
-- The unique index keeps one current row per directed pair, so re-requests
-- after rejection update in place. Pending/accepted are idempotent (the
-- WHERE clause filters them out, leaving DO UPDATE a no-op). Blocked is
-- terminal (the WHERE clause omits it). See migration 0012.
INSERT INTO friend_edges (tenant_id, from_user_id, to_user_id, status)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, 'pending'
)
ON CONFLICT (tenant_id, from_user_id, to_user_id)
DO UPDATE SET status     = 'pending',
              updated_at = now()
WHERE friend_edges.status = 'rejected'
RETURNING id, status;

-- name: GetFriendEdge :one
SELECT id, status
FROM friend_edges
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND from_user_id = $1
  AND to_user_id   = $2;

-- name: SetFriendEdgeStatus :exec
UPDATE friend_edges
SET status     = $3,
    updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND from_user_id = $1
  AND to_user_id   = $2;

-- name: DeleteFriendEdge :execrows
-- Symmetric unfriend: the caller can be on either side of the edge. The
-- previous one-directional query silently no-op'd when the friendship was
-- inbound, so a "delete" returned 204 without actually removing anything.
DELETE FROM friend_edges
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND ((from_user_id = sqlc.arg('me') AND to_user_id = sqlc.arg('other'))
       OR (from_user_id = sqlc.arg('other') AND to_user_id = sqlc.arg('me')));

-- name: ListFriendsByStatusForCaller :many
-- Caller-aware list. For 'blocked', only rows where the caller initiated
-- the block are returned — never rows where the caller is the blockee
-- (which would let a blocked user enumerate who blocked them).
SELECT id, from_user_id, to_user_id, status, created_at, updated_at
FROM friend_edges
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND ((sqlc.arg('status')::text != 'blocked'
            AND (from_user_id = sqlc.arg('me') OR to_user_id = sqlc.arg('me')))
       OR (sqlc.arg('status')::text = 'blocked'
            AND from_user_id = sqlc.arg('me')))
  AND status = sqlc.arg('status')
  AND id > sqlc.arg('cursor')
ORDER BY id ASC
LIMIT sqlc.arg('row_limit');

-- name: ListFriendsByStatus :many
SELECT id, from_user_id, to_user_id, status, created_at, updated_at
FROM friend_edges
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND (from_user_id = $1 OR to_user_id = $1)
  AND status = $2
  AND id > $3
ORDER BY id ASC
LIMIT $4;

-- name: ListAcceptedFriendIDs :many
-- Returns (from_user_id, to_user_id) pairs for all accepted friendships
-- involving the given user. Callers resolve the "other" user by comparing
-- each column against their own ID.
SELECT from_user_id, to_user_id
FROM friend_edges
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND (from_user_id = $1 OR to_user_id = $1)
  AND status = 'accepted';

-- name: AreFriendsAccepted :one
-- Returns the edge ID if an accepted friendship exists between the two
-- users in either direction; pgx.ErrNoRows if they are not friends.
SELECT id FROM friend_edges
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND ((from_user_id = $1 AND to_user_id = $2)
       OR (from_user_id = $2 AND to_user_id = $1))
  AND status = 'accepted'
LIMIT 1;

-- name: ListEndUserIdentitiesForUsers :many
-- Bulk-fetch email + xuid for a set of users, used to enrich friend lists.
-- email is COALESCEd because anonymous users have none.
SELECT id AS end_user_id, COALESCE(email::text, '')::text AS email, xuid
FROM end_users
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = ANY(sqlc.arg('end_user_ids')::bigint[]);
