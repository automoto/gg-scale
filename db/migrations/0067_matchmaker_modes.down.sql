CREATE OR REPLACE FUNCTION notify_matchmaker_ticket() RETURNS TRIGGER AS $$
BEGIN
    PERFORM pg_notify('matchmaker_ticket', json_build_object(
        'tenant_id',  NEW.tenant_id,
        'project_id', NEW.project_id,
        'fleet_id',   NEW.fleet_id,
        'region',     NEW.region,
        'game_mode',  NEW.game_mode
    )::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP INDEX IF EXISTS matchmaking_tickets_queued_idx;
CREATE INDEX matchmaking_tickets_queued_idx
    ON matchmaking_tickets (tenant_id, project_id, fleet_id, region, game_mode, created_at, id)
    WHERE status = 'queued' AND claim_id IS NULL;

ALTER TABLE matchmaking_tickets
    DROP CONSTRAINT matchmaking_tickets_fleet_mode,
    DROP CONSTRAINT matchmaking_tickets_count_range;

ALTER TABLE matchmaking_tickets
    DROP COLUMN mode,
    DROP COLUMN match_id,
    DROP COLUMN min_count,
    DROP COLUMN max_count,
    DROP COLUMN count_multiple,
    DROP COLUMN allow_cross_region,
    DROP COLUMN query,
    DROP COLUMN string_properties,
    DROP COLUMN numeric_properties,
    DROP COLUMN expires_at;

ALTER TABLE matchmaking_tickets
    ADD CONSTRAINT matchmaking_tickets_fleet_id_required
    CHECK (fleet_id IS NOT NULL)
    NOT VALID;
