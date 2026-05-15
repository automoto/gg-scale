-- name: InsertMatchmakingTicket :one
INSERT INTO matchmaking_tickets (
    tenant_id, project_id, end_user_id, region, game_mode, attributes
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, $4, $5
)
RETURNING id, status::text AS status, created_at;

-- name: GetMatchmakingTicket :one
SELECT id, tenant_id, project_id, end_user_id, region, game_mode,
       attributes, status::text AS status, match_address,
       created_at, matched_at
FROM matchmaking_tickets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1;

-- name: CancelMatchmakingTicket :one
UPDATE matchmaking_tickets
SET status = 'cancelled'
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = $1
  AND status = 'queued'
RETURNING id;

-- name: ListReadyMatchmakerBuckets :many
SELECT tenant_id, project_id, region, game_mode, count(*) AS ticket_count
FROM matchmaking_tickets
WHERE status = 'queued'
GROUP BY tenant_id, project_id, region, game_mode
HAVING count(*) >= $1::int
ORDER BY tenant_id, project_id, region, game_mode;

-- name: PopMatchmakerBucket :many
WITH candidates AS (
    SELECT mt.id
    FROM matchmaking_tickets mt
    WHERE mt.status = 'queued'
      AND mt.tenant_id = $1
      AND mt.project_id = $2
      AND mt.region = $3
      AND mt.game_mode = $4
    ORDER BY mt.created_at, mt.id
    LIMIT $5::int
    FOR UPDATE SKIP LOCKED
)
UPDATE matchmaking_tickets t
SET status = 'matched'
FROM candidates c
WHERE t.id = c.id
RETURNING t.id, t.tenant_id, t.project_id, t.end_user_id, t.region,
          t.game_mode, t.attributes, t.status::text AS status,
          t.match_address, t.created_at, t.matched_at;

-- name: MarkMatchmakerMatched :exec
UPDATE matchmaking_tickets
SET match_address = $2,
    matched_at    = now()
WHERE id = ANY($1::bigint[]);

-- name: MarkMatchmakerFailed :exec
UPDATE matchmaking_tickets
SET status = 'failed'
WHERE id = ANY($1::bigint[]);

-- name: ListMatchmakerBucketsForProject :many
-- Dashboard matchmaker page: queue depth per (region, game_mode) bucket for
-- the current tenant's project, plus oldest queued ticket so operators can
-- spot stuck buckets at a glance.
SELECT region,
       game_mode,
       status::text AS status,
       count(*)::bigint AS ticket_count,
       min(created_at)::timestamptz AS oldest
FROM matchmaking_tickets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
GROUP BY region, game_mode, status
ORDER BY region, game_mode, status;
