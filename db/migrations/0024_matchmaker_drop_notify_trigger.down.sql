-- Restore the in-transaction NOTIFY trigger (baseline definition). With this
-- back in place the application post-commit notify becomes a redundant second
-- wakeup; the debounce keeps it cheap, but roll back the app change too.
CREATE FUNCTION public.notify_matchmaker_ticket() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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
$$;

CREATE TRIGGER matchmaking_tickets_notify AFTER INSERT ON public.matchmaking_tickets FOR EACH ROW WHEN ((new.status = 'queued'::public.ticket_status)) EXECUTE FUNCTION public.notify_matchmaker_ticket();
