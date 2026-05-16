-- Update the matchmaker notify trigger to include fleet_id so the worker's
-- Bucket key fully identifies the queue (tickets for different fleets must
-- never merge). Same dedup-friendly shape: small JSON, no per-row dynamic
-- fields.

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
