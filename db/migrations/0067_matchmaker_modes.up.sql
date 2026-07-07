-- Matchmaking becomes fleet-independent. Tickets carry a result mode
-- (match_only / game_session / fleet_allocation), per-ticket group sizing,
-- queryable properties, and an expiry. fleet_id is only required for
-- fleet_allocation.

ALTER TABLE matchmaking_tickets
    DROP CONSTRAINT matchmaking_tickets_fleet_id_required;

ALTER TABLE matchmaking_tickets
    ADD COLUMN mode TEXT NOT NULL DEFAULT 'fleet_allocation'
        CHECK (mode IN ('match_only', 'game_session', 'fleet_allocation')),
    ADD COLUMN match_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN min_count INT NOT NULL DEFAULT 1 CHECK (min_count >= 1),
    ADD COLUMN max_count INT NOT NULL DEFAULT 1,
    ADD COLUMN count_multiple INT NOT NULL DEFAULT 1 CHECK (count_multiple >= 1),
    ADD COLUMN allow_cross_region BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN query TEXT NOT NULL DEFAULT '*',
    ADD COLUMN string_properties JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN numeric_properties JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN expires_at TIMESTAMPTZ;

ALTER TABLE matchmaking_tickets
    ADD CONSTRAINT matchmaking_tickets_count_range CHECK (max_count >= min_count),
    ADD CONSTRAINT matchmaking_tickets_fleet_mode
        CHECK (mode <> 'fleet_allocation' OR fleet_id IS NOT NULL);

-- Rebuild the worker-scan index over the full bucket key.
DROP INDEX IF EXISTS matchmaking_tickets_queued_idx;
CREATE INDEX matchmaking_tickets_queued_idx
    ON matchmaking_tickets (tenant_id, project_id, mode, fleet_id, region, game_mode, created_at, id)
    WHERE status = 'queued' AND claim_id IS NULL;

-- NOTIFY payload now carries the mode; fleet_id is null for non-fleet
-- tickets. Same dedup-friendly shape as 0033.
CREATE OR REPLACE FUNCTION notify_matchmaker_ticket() RETURNS TRIGGER AS $$
BEGIN
    PERFORM pg_notify('matchmaker_ticket', json_build_object(
        'tenant_id',  NEW.tenant_id,
        'project_id', NEW.project_id,
        'mode',       NEW.mode,
        'fleet_id',   NEW.fleet_id,
        'region',     NEW.region,
        'game_mode',  NEW.game_mode
    )::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
