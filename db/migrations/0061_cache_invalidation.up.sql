-- Cache invalidations are emitted in the writing transaction and delivered only
-- on commit. This covers application writes and out-of-band administrative SQL;
-- listener reconnects flush every cache to cover notifications missed while down.
CREATE FUNCTION ggscale_cache_invalidation_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cache_kind TEXT;
    subject_id BIGINT;
BEGIN
    IF TG_TABLE_NAME = 'project_players' THEN
        cache_kind := 'session_epoch';
        IF TG_OP = 'DELETE' THEN
            subject_id := OLD.id;
        ELSE
            subject_id := NEW.id;
        END IF;
        PERFORM pg_notify(
            'ggscale_cache_invalidation',
            json_build_object('kind', cache_kind, 'player_id', subject_id)::text
        );
        RETURN NULL;
    END IF;

    IF TG_TABLE_NAME NOT IN ('api_keys', 'tenants', 'connection_limit_overrides', 'rate_limit_overrides') THEN
        RAISE EXCEPTION 'unexpected cache invalidation table: %', TG_TABLE_NAME;
    END IF;
    IF TG_TABLE_NAME = 'rate_limit_overrides' THEN
        cache_kind := 'override';
    ELSE
        cache_kind := 'api_key';
    END IF;
    IF TG_TABLE_NAME = 'tenants' THEN
        IF TG_OP = 'DELETE' THEN
            subject_id := OLD.id;
        ELSE
            subject_id := NEW.id;
        END IF;
    ELSIF TG_OP = 'DELETE' THEN
        subject_id := OLD.tenant_id;
    ELSE
        subject_id := NEW.tenant_id;
    END IF;
    PERFORM pg_notify(
        'ggscale_cache_invalidation',
        json_build_object('kind', cache_kind, 'tenant_id', subject_id)::text
    );
    RETURN NULL;
END;
$$;

CREATE TRIGGER project_players_cache_invalidation
AFTER UPDATE OF session_epoch OR DELETE ON project_players
FOR EACH ROW EXECUTE FUNCTION ggscale_cache_invalidation_notify();

CREATE TRIGGER api_keys_cache_invalidation
AFTER INSERT OR UPDATE OR DELETE ON api_keys
FOR EACH ROW EXECUTE FUNCTION ggscale_cache_invalidation_notify();

CREATE TRIGGER tenants_cache_invalidation
AFTER UPDATE OF disabled_at, tier OR DELETE ON tenants
FOR EACH ROW EXECUTE FUNCTION ggscale_cache_invalidation_notify();

CREATE TRIGGER connection_limit_overrides_cache_invalidation
AFTER INSERT OR UPDATE OR DELETE ON connection_limit_overrides
FOR EACH ROW EXECUTE FUNCTION ggscale_cache_invalidation_notify();

CREATE TRIGGER rate_limit_overrides_cache_invalidation
AFTER INSERT OR UPDATE OR DELETE ON rate_limit_overrides
FOR EACH ROW EXECUTE FUNCTION ggscale_cache_invalidation_notify();
