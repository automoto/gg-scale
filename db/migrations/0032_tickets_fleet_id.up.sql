-- Each matchmaking ticket carries the fleet it should be allocated against.
-- The bucket key the worker uses is widened to include fleet_id so tickets
-- for different fleets queue independently (ranked vs casual, doomerang vs
-- trivia, etc.).

ALTER TABLE matchmaking_tickets
    ADD COLUMN fleet_id BIGINT REFERENCES fleets(id) ON DELETE RESTRICT;

-- Replace the bucket-scan index so the worker partitions by fleet_id too.
DROP INDEX IF EXISTS matchmaking_tickets_queued_idx;
CREATE INDEX matchmaking_tickets_queued_idx
    ON matchmaking_tickets (fleet_id, region, game_mode, created_at)
    WHERE status = 'queued';
