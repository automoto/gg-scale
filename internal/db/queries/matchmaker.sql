-- name: InsertMatchmakingTicket :one
INSERT INTO matchmaking_tickets (
    tenant_id, project_id, fleet_id, end_user_id, region, game_mode, attributes
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, $4, $5, $6
)
RETURNING id, status::text AS status, created_at;

-- name: GetMatchmakingTicket :one
SELECT id, tenant_id, project_id, fleet_id, end_user_id, region, game_mode,
       attributes, status::text AS status, match_address, match_protocol,
       created_at, matched_at
FROM matchmaking_tickets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg(id)
  AND end_user_id = sqlc.arg(end_user_id);

-- name: CancelMatchmakingTicket :one
-- Cancelling a claimed-but-not-yet-committed ticket is allowed: the worker's
-- CommitClaim will find zero rows and deallocate the orphan server.
UPDATE matchmaking_tickets
SET status           = 'cancelled',
    claim_id         = NULL,
    claimed_at       = NULL,
    claim_expires_at = NULL
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg(id)
  AND end_user_id = sqlc.arg(end_user_id)
  AND status = 'queued'
RETURNING id;

-- name: ListReadyMatchmakerBuckets :many
SELECT tenant_id, project_id, fleet_id, region, game_mode, count(*) AS ticket_count
FROM matchmaking_tickets
WHERE status = 'queued'
  AND claim_id IS NULL
  AND fleet_id IS NOT NULL
GROUP BY tenant_id, project_id, fleet_id, region, game_mode
HAVING count(*) >= $1::int
ORDER BY tenant_id, project_id, fleet_id, region, game_mode;

-- name: ClaimMatchmakerBucket :many
-- Stake a claim on up to N unclaimed queued tickets in the bucket. The rows
-- stay 'queued'; only claim_id/claimed_at/claim_expires_at are set, so a
-- subsequent ClaimBucket (different worker) skips them. The caller commits
-- via CommitMatchmakerClaim (success) or ReleaseMatchmakerClaim (failure);
-- a crashed caller's claim is released by the sweeper once
-- claim_expires_at < now().
WITH candidates AS (
    SELECT mt.id
    FROM matchmaking_tickets mt
    WHERE mt.status = 'queued'
      AND mt.claim_id IS NULL
      AND mt.tenant_id  = $1
      AND mt.project_id = $2
      AND mt.fleet_id   = $3
      AND mt.region     = $4
      AND mt.game_mode  = $5
    ORDER BY mt.created_at, mt.id
    LIMIT sqlc.arg('limit')::int
    FOR UPDATE SKIP LOCKED
)
UPDATE matchmaking_tickets t
SET claim_id         = sqlc.arg('claim_id')::uuid,
    claimed_at       = now(),
    claim_expires_at = now() + sqlc.arg('ttl')::interval
FROM candidates c
WHERE t.id = c.id
RETURNING t.id, t.tenant_id, t.project_id, t.fleet_id, t.end_user_id, t.region,
          t.game_mode, t.attributes, t.status::text AS status,
          t.match_address, t.match_protocol, t.created_at, t.matched_at;

-- name: CommitMatchmakerClaim :execrows
-- Flip every still-queued ticket holding this claim_id to 'matched' and
-- stamp the address + protocol. Rows that drifted (cancelled, swept)
-- won't match the WHERE and are excluded — the caller branches on
-- rows-affected and deallocates the orphan server when 0.
UPDATE matchmaking_tickets
SET status           = 'matched',
    match_address    = sqlc.arg('match_address'),
    match_protocol   = sqlc.arg('match_protocol'),
    matched_at       = now(),
    claim_id         = NULL,
    claimed_at       = NULL,
    claim_expires_at = NULL
WHERE claim_id = sqlc.arg('claim_id')::uuid
  AND status = 'queued';

-- name: ReleaseMatchmakerClaim :execrows
-- Worker-driven release: allocator failed (or the worker is giving up).
-- Bump allocation_attempts; flip to 'failed' on the Nth attempt.
UPDATE matchmaking_tickets
SET claim_id            = NULL,
    claimed_at          = NULL,
    claim_expires_at    = NULL,
    allocation_attempts = allocation_attempts + 1,
    status = CASE
        WHEN allocation_attempts + 1 >= sqlc.arg('max_attempts')::int
            THEN 'failed'::ticket_status
        ELSE status
    END
WHERE claim_id = sqlc.arg('claim_id')::uuid
  AND status = 'queued';

-- name: SweepStaleMatchmakerClaims :execrows
-- Release every claim whose lease has expired. Same accounting as
-- ReleaseMatchmakerClaim (bump attempts, fail at the cap). Runs out of a
-- detached context so it isn't tied to any request lifetime.
UPDATE matchmaking_tickets
SET claim_id            = NULL,
    claimed_at          = NULL,
    claim_expires_at    = NULL,
    allocation_attempts = allocation_attempts + 1,
    status = CASE
        WHEN allocation_attempts + 1 >= sqlc.arg('max_attempts')::int
            THEN 'failed'::ticket_status
        ELSE status
    END
WHERE claim_id IS NOT NULL
  AND status = 'queued'
  AND claim_expires_at < now();

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
