-- Production grants for the ggscale_app role created in 0007. The runtime
-- connection user is a member of ggscale_app (or assumes it via SET ROLE),
-- which makes RLS apply at runtime — superuser/owner bypass would defeat
-- the policies in 0009.
--
-- audit_log keeps its narrow INSERT+SELECT grant from 0007 so the app
-- cannot rewrite history.

GRANT SELECT, INSERT, UPDATE, DELETE ON
    tenants, projects, api_keys,
    end_users, sessions,
    storage_objects,
    leaderboards, leaderboard_entries,
    friend_edges,
    usage_samples
TO ggscale_app;

GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ggscale_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO ggscale_app;

-- Bootstrap policy: the tenant middleware looks up an api_key by hash to
-- discover the tenant_id, BEFORE any tenant context exists. With only the
-- isolation policy, that lookup would see zero rows. This permissive policy
-- adds an OR clause: SELECT is allowed when the GUC is unset/empty. Once
-- middleware sets app.tenant_id for the request, the original isolation
-- policy filters every subsequent query.
CREATE POLICY api_keys_bootstrap ON api_keys
    FOR SELECT
    USING (
        current_setting('app.tenant_id', true) IS NULL
        OR current_setting('app.tenant_id', true) = ''
    );
