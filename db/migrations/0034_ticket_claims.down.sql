DROP INDEX IF EXISTS matchmaking_tickets_claim_expiry_idx;
DROP INDEX IF EXISTS matchmaking_tickets_queued_idx;
CREATE INDEX matchmaking_tickets_queued_idx
    ON matchmaking_tickets (fleet_id, region, game_mode, created_at)
    WHERE status = 'queued';

ALTER TABLE matchmaking_tickets
    DROP COLUMN allocation_attempts,
    DROP COLUMN claim_expires_at,
    DROP COLUMN claimed_at,
    DROP COLUMN claim_id;
