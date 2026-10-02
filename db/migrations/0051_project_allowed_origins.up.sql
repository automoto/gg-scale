-- Per-project browser origins for CORS and the realtime WebSocket. A browser
-- preflight carries no API key, so the server cannot know the project at
-- that time: an origin is accepted if any live project lists it, or if
-- CORS_ALLOWED_ORIGINS lists it. CORS is not the access control here; the
-- API key is.
ALTER TABLE projects
    ADD COLUMN allowed_origins TEXT[] NOT NULL DEFAULT '{}';

-- The origin cache reads the lists of all projects with no tenant set, and
-- projects has only the tenant isolation policy. This function returns the
-- origins and nothing else.
CREATE FUNCTION public.all_project_allowed_origins() RETURNS SETOF text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
    SELECT DISTINCT unnest(allowed_origins)
    FROM projects
    WHERE deleted_at IS NULL;
$$;

REVOKE ALL ON FUNCTION public.all_project_allowed_origins() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.all_project_allowed_origins() TO ggscale_app;
