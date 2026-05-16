-- Two-phase matchmaker claim. A worker first stakes a claim on a set of
-- queued tickets (status stays 'queued', claim_id/claimed_at/claim_expires_at
-- are set), then calls Allocate, then commits the claim by flipping the rows
-- to 'matched'. If the worker crashes between claim and commit — or the
-- allocator takes longer than claim_expires_at — the sweeper releases the
-- claim, bumps allocation_attempts, and either retries or fails the ticket.
--
-- This replaces the previous PopBucket pattern, which flipped rows to
-- 'matched' before the allocator ran. That pattern silently stranded tickets
-- on short-count races (C1) and could orphan game servers when MarkMatched
-- failed after Allocate (M3).

ALTER TABLE matchmaking_tickets
    ADD COLUMN claim_id            UUID,
    ADD COLUMN claimed_at          TIMESTAMPTZ,
    ADD COLUMN claim_expires_at    TIMESTAMPTZ,
    ADD COLUMN allocation_attempts INT NOT NULL DEFAULT 0;

-- The bucket scan now wants only unclaimed queued tickets. Tighten the
-- partial index to skip rows that another worker has staked.
DROP INDEX IF EXISTS matchmaking_tickets_queued_idx;
CREATE INDEX matchmaking_tickets_queued_idx
    ON matchmaking_tickets (tenant_id, project_id, fleet_id, region, game_mode, created_at, id)
    WHERE status = 'queued' AND claim_id IS NULL;

-- Sweeper hits this index when scanning for expired claims.
CREATE INDEX matchmaking_tickets_claim_expiry_idx
    ON matchmaking_tickets (claim_expires_at)
    WHERE claim_id IS NOT NULL;
