-- Per-project player deletion: account-portal request/cancel plus the purge
-- sweep. The portal queries run under BootstrapQ with app.tenant_id set for
-- the link's tenant (same pattern as UnlinkPlayerFromAccount); the sweep runs
-- per tenant under Pool.Q. The account guard stops one account from deleting
-- another account's player.

-- name: RequestPlayerDeleteByAccount :one
-- Portal-side delete request: disables the player (keeping an earlier
-- suspension timestamp intact) and stamps delete_requested_at with the same
-- now() so cancel can tell the two apart. 0 rows = not this account's player
-- or already pending.
UPDATE project_players
SET delete_requested_at = now(),
    disabled_at   = COALESCE(disabled_at, now()),
    session_epoch = session_epoch + 1
WHERE id = sqlc.arg(id)
  AND player_account_id = sqlc.arg(player_account_id)
  AND deleted_at IS NULL
  AND delete_requested_at IS NULL
RETURNING delete_requested_at;

-- name: CancelPlayerDeleteByAccount :execrows
-- Clears the pending request; lifts the disable only when the request created
-- it (disabled_at = delete_requested_at), so a pre-existing admin suspension
-- survives the cancel. 0 rows = no pending request (or already purged).
UPDATE project_players
SET disabled_at = CASE WHEN disabled_at = delete_requested_at THEN NULL ELSE disabled_at END,
    delete_requested_at = NULL
WHERE id = sqlc.arg(id)
  AND player_account_id = sqlc.arg(player_account_id)
  AND deleted_at IS NULL
  AND delete_requested_at IS NOT NULL;

-- name: GetPlayerDeleteRequestedByAccount :one
-- Race repair for the idempotent portal request: when the guarded UPDATE
-- matched no rows because another surface scheduled the deletion first, this
-- reads the timestamp that request stored.
SELECT delete_requested_at
FROM project_players
WHERE id = sqlc.arg(id)
  AND player_account_id = sqlc.arg(player_account_id)
  AND deleted_at IS NULL;

-- name: DeletePlayerGameSessionSignals :exec
-- from/to player columns carry no foreign keys, and signals in a surviving
-- session outlive the player-row cascade. Remove every signal the purged
-- players sent or were addressed by, so no player IDs or SDP payloads remain
-- after the purge (the 2-minute TTL GC is not guaranteed to have run).
DELETE FROM game_session_signal
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND (from_player_id = ANY(sqlc.arg(ids)::bigint[])
       OR to_player_id = ANY(sqlc.arg(ids)::bigint[]));

-- name: ListPlayersDueForPurge :many
-- One purge batch for the current tenant. FOR UPDATE serializes against a
-- racing cancel: the cancel either lands first (the row drops out of the
-- result) or blocks until the delete commits and reports "no pending
-- deletion". deleted_at is returned so the caller only releases quota slots
-- for rows that ever held one.
SELECT id, project_id, external_id, delete_requested_at, deleted_at
FROM project_players
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND delete_requested_at IS NOT NULL
  AND delete_requested_at <= sqlc.arg(cutoff)::timestamptz
ORDER BY id
LIMIT sqlc.arg(batch)
FOR UPDATE;

-- name: HardDeletePlayers :execrows
-- The point of no return: FK cascades remove sessions, presence, leaderboard
-- entries, storage objects, matchmaking tickets, invites, and hosted game
-- sessions; audit_log rows survive with actor_user_id set NULL.
DELETE FROM project_players
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = ANY(sqlc.arg(ids)::bigint[]);
