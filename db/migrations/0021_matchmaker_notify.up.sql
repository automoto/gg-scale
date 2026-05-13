-- Wake the matchmaker worker on queued ticket inserts so it doesn't have
-- to poll. Payload is JSON so future region/game_mode values containing
-- ':' don't break the parser. Well under Postgres's 8000-byte NOTIFY cap.
--
-- Do NOT add per-row dynamic fields (created_at, uuid, …) to the payload.
-- Postgres deduplicates identical NOTIFY payloads within a transaction,
-- which keeps a 100-row bulk insert into the same bucket to a single
-- NOTIFY. Distinct payloads per row would defeat dedup and spam listeners.

CREATE OR REPLACE FUNCTION notify_matchmaker_ticket() RETURNS TRIGGER AS $$
BEGIN
    PERFORM pg_notify('matchmaker_ticket', json_build_object(
        'tenant_id',  NEW.tenant_id,
        'project_id', NEW.project_id,
        'region',     NEW.region,
        'game_mode',  NEW.game_mode
    )::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER matchmaking_tickets_notify
    AFTER INSERT ON matchmaking_tickets
    FOR EACH ROW
    WHEN (NEW.status = 'queued')
    EXECUTE FUNCTION notify_matchmaker_ticket();
