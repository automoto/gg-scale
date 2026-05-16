DROP INDEX IF EXISTS matchmaking_tickets_queued_idx;
CREATE INDEX matchmaking_tickets_queued_idx
    ON matchmaking_tickets (region, game_mode, created_at)
    WHERE status = 'queued';

ALTER TABLE matchmaking_tickets DROP COLUMN IF EXISTS fleet_id;
