-- name: CreateRealtimeTicket :exec
INSERT INTO realtime_tickets (
    tenant_id, ticket_hash, api_key_hash, player_id, project_id, session_epoch, expires_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(ticket_hash), sqlc.arg(api_key_hash), sqlc.arg(player_id),
    sqlc.arg(project_id), sqlc.arg(session_epoch), sqlc.arg(expires_at)
);

-- name: RedeemRealtimeTicket :one
-- Single use: the row is deleted as it is read. Runs with no tenant set.
DELETE FROM realtime_tickets
WHERE ticket_hash = sqlc.arg(ticket_hash)
  AND expires_at > now()
RETURNING tenant_id, api_key_hash, player_id, project_id, session_epoch;

-- name: DeleteExpiredRealtimeTickets :execrows
DELETE FROM realtime_tickets
WHERE expires_at <= now();
