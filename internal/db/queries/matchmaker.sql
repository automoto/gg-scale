-- name: InsertMatchmakingTicket :one
INSERT INTO matchmaking_tickets (
    tenant_id, project_id, fleet_id, player_id, region, game_mode, attributes,
    mode, min_count, max_count, count_multiple, allow_cross_region,
    query, string_properties, numeric_properties, expires_at
)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
)
RETURNING id, status::text AS status, created_at;

-- name: GetMatchmakingTicket :one
SELECT id, tenant_id, project_id, fleet_id, player_id, region, game_mode,
       attributes, status::text AS status, match_address, match_protocol,
       mode, match_id, min_count, max_count, count_multiple,
       allow_cross_region, query, string_properties, numeric_properties,
       created_at, matched_at, expires_at
FROM matchmaking_tickets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg(id)
  AND player_id = sqlc.arg(player_id);

-- name: CountQueuedTicketsForPlayer :one
-- Concurrent-ticket cap: how many live queued tickets the player already has
-- in the project. Expired-but-unswept tickets don't count against the cap.
SELECT count(*)
FROM matchmaking_tickets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
  AND player_id = sqlc.arg(player_id)
  AND status = 'queued'
  AND (expires_at IS NULL OR expires_at > now());

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
  AND player_id = sqlc.arg(player_id)
  AND status = 'queued'
RETURNING id;

-- name: ListReadyMatchmakerBuckets :many
-- Region is a bucket dimension only for fleet_allocation (the server must
-- be placed in a concrete region); non-fleet modes mix regions inside one
-- bucket and the worker applies the soft-region grouping rules in Go.
SELECT tenant_id, project_id, mode, fleet_id,
       (CASE WHEN mode = 'fleet_allocation' THEN region ELSE '' END)::text AS region,
       game_mode, count(*) AS ticket_count
FROM matchmaking_tickets
WHERE status = 'queued'
  AND claim_id IS NULL
  AND (expires_at IS NULL OR expires_at > now())
GROUP BY tenant_id, project_id, mode, fleet_id,
         CASE WHEN mode = 'fleet_allocation' THEN region ELSE '' END,
         game_mode
ORDER BY tenant_id, project_id, mode, fleet_id, region, game_mode;

-- name: ClaimMatchmakerBucket :many
-- Stake a claim on up to N unclaimed queued tickets in the bucket. The rows
-- stay 'queued'; only claim_id/claimed_at/claim_expires_at are set, so a
-- subsequent ClaimBucket (different worker) skips them. The caller commits
-- via CommitMatchmakerClaim (success) or ReleaseMatchmakerClaim (failure);
-- a crashed caller's claim is released by the sweeper once
-- claim_expires_at < now(). fleet_id is NULL for non-fleet modes, hence
-- IS NOT DISTINCT FROM.
WITH candidates AS (
    SELECT mt.id
    FROM matchmaking_tickets mt
    WHERE mt.status = 'queued'
      AND mt.claim_id IS NULL
      AND (mt.expires_at IS NULL OR mt.expires_at > now())
      AND mt.tenant_id  = sqlc.arg(tenant_id)
      AND mt.project_id = sqlc.arg(project_id)
      AND mt.mode       = sqlc.arg(mode)
      AND mt.fleet_id IS NOT DISTINCT FROM sqlc.narg(fleet_id)::bigint
      AND (mt.mode <> 'fleet_allocation' OR mt.region = sqlc.arg(region))
      AND mt.game_mode  = sqlc.arg(game_mode)
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
RETURNING t.id, t.tenant_id, t.project_id, t.fleet_id, t.player_id, t.region,
          t.game_mode, t.attributes, t.status::text AS status,
          t.match_address, t.match_protocol, t.mode, t.min_count, t.max_count,
          t.count_multiple, t.allow_cross_region, t.query,
          t.string_properties, t.numeric_properties, t.created_at, t.matched_at;

-- name: CommitMatchmakerTickets :execrows
-- Flip the given still-queued tickets holding this claim_id to 'matched'
-- and stamp the match id, address + protocol. Rows that drifted (cancelled,
-- swept) won't match the WHERE and are excluded — the caller compares
-- rows-affected and deallocates the orphan server when 0.
UPDATE matchmaking_tickets
SET status           = 'matched',
    match_id         = sqlc.arg('match_id'),
    match_address    = sqlc.arg('match_address'),
    match_protocol   = sqlc.arg('match_protocol'),
    matched_at       = now(),
    claim_id         = NULL,
    claimed_at       = NULL,
    claim_expires_at = NULL
WHERE claim_id = sqlc.arg('claim_id')::uuid
  AND id = ANY (sqlc.arg(ticket_ids)::bigint[])
  AND status = 'queued';

-- name: ReleaseMatchmakerTickets :execrows
-- Worker-driven release of one failed group: the resolver (allocator,
-- session creator) failed. Bump allocation_attempts; flip to 'failed' on
-- the Nth attempt.
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
  AND id = ANY (sqlc.arg(ticket_ids)::bigint[])
  AND status = 'queued';

-- name: ReturnMatchmakerClaim :execrows
-- Un-claim whatever the claim still holds without penalty: tickets that
-- didn't fit a group this pass simply go back to waiting.
UPDATE matchmaking_tickets
SET claim_id         = NULL,
    claimed_at       = NULL,
    claim_expires_at = NULL
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

-- name: ExpireMatchmakerTickets :execrows
-- TTL enforcement: unclaimed queued tickets past expires_at flip to
-- 'failed'. Claimed tickets are left alone — the claim path settles them.
UPDATE matchmaking_tickets
SET status = 'failed'
WHERE status = 'queued'
  AND claim_id IS NULL
  AND expires_at IS NOT NULL
  AND expires_at < now();

-- name: ListMatchmakerBucketsForProject :many
-- Control panel matchmaker page: queue depth per (mode, region, game_mode)
-- bucket for the current tenant's project, plus oldest queued ticket and
-- the min/max count spread so operators can spot stuck buckets at a glance.
SELECT mode,
       region,
       game_mode,
       status::text AS status,
       count(*)::bigint AS ticket_count,
       min(created_at)::timestamptz AS oldest,
       min(min_count)::int AS min_count_low,
       max(max_count)::int AS max_count_high
FROM matchmaking_tickets
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
GROUP BY mode, region, game_mode, status
ORDER BY mode, region, game_mode, status;

-- name: CountMatchmakerMatchesByMode :many
-- Control panel matchmaker page: matches formed per mode within the retention
-- window (rows are GC'd after MatchTTL, so this reads as "recent matches").
SELECT mode, count(*)::bigint AS match_count
FROM matchmaker_matches
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id = sqlc.arg(project_id)
GROUP BY mode
ORDER BY mode;

-- name: InsertMatchmakerMatch :exec
INSERT INTO matchmaker_matches (
    id, tenant_id, project_id, mode, fleet_id, address, protocol,
    session_id, join_code, roster, expires_at
)
VALUES (
    sqlc.arg(id),
    current_setting('app.tenant_id', true)::bigint,
    sqlc.arg(project_id), sqlc.arg(mode), sqlc.narg(fleet_id),
    sqlc.arg(address), sqlc.arg(protocol), sqlc.arg(session_id),
    sqlc.arg(join_code), sqlc.arg(roster), sqlc.arg(expires_at)
);

-- name: GetMatchmakerMatch :one
SELECT id, tenant_id, project_id, mode, fleet_id, address, protocol,
       session_id, join_code, roster, created_at, expires_at
FROM matchmaker_matches
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND id = sqlc.arg(id);

-- name: DeleteExpiredMatchmakerMatches :execrows
-- GC (River job, leader-elected): drop match rows past their retention
-- window. Privileged — runs without a tenant GUC.
DELETE FROM matchmaker_matches
WHERE expires_at < now();

-- name: DeleteTerminalMatchmakerTickets :execrows
-- GC (River job, leader-elected): drop matched/cancelled/failed tickets
-- older than the retention interval. Privileged — runs without a tenant GUC.
-- Anchored on when the ticket became terminal (matched_at for matched
-- tickets, created_at otherwise) so a matched ticket isn't purged before its
-- paired match row's retention window — otherwise a poll would 404 while the
-- match is still recoverable.
DELETE FROM matchmaking_tickets
WHERE status <> 'queued'
  AND COALESCE(matched_at, created_at) < now() - sqlc.arg(retention)::interval;
