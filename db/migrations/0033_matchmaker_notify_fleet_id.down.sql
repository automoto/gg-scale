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
