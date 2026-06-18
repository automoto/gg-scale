CREATE OR REPLACE FUNCTION dashboard_create_tenant(
    p_actor_user_id BIGINT,
    p_tenant_name TEXT,
    p_project_name TEXT,
    p_key_hash BYTEA,
    p_key_label TEXT
)
RETURNS TABLE (
    tenant_id BIGINT,
    project_id BIGINT,
    api_key_id BIGINT,
    membership_id BIGINT
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_label TEXT := nullif(trim(p_key_label), '');
BEGIN
    IF p_actor_user_id IS NULL OR p_actor_user_id <= 0 THEN
        RAISE EXCEPTION 'dashboard actor user id is required' USING ERRCODE = '22023';
    END IF;
    IF nullif(trim(p_tenant_name), '') IS NULL THEN
        RAISE EXCEPTION 'tenant name is required' USING ERRCODE = '22023';
    END IF;
    IF nullif(trim(p_project_name), '') IS NULL THEN
        RAISE EXCEPTION 'project name is required' USING ERRCODE = '22023';
    END IF;
    IF p_key_hash IS NULL OR length(p_key_hash) = 0 THEN
        RAISE EXCEPTION 'api key hash is required' USING ERRCODE = '22023';
    END IF;

    PERFORM set_config('app.allow_tenant_bootstrap', '1', true);

    INSERT INTO tenants (name)
    VALUES (trim(p_tenant_name))
    RETURNING id INTO tenant_id;

    PERFORM set_config('app.tenant_id', tenant_id::TEXT, true);

    INSERT INTO projects (tenant_id, name)
    VALUES (tenant_id, trim(p_project_name))
    RETURNING id INTO project_id;

    INSERT INTO api_keys (tenant_id, project_id, key_hash, label, scopes)
    VALUES (tenant_id, project_id, p_key_hash, v_label, '{}'::TEXT[])
    RETURNING id INTO api_key_id;

    INSERT INTO dashboard_memberships (dashboard_user_id, tenant_id, role)
    VALUES (p_actor_user_id, tenant_id, 'owner')
    RETURNING id INTO membership_id;

    INSERT INTO audit_log (tenant_id, action, target, payload)
    VALUES (
        tenant_id,
        'dashboard.tenant.created',
        'tenant:' || tenant_id::TEXT,
        jsonb_build_object(
            'dashboard_user_id', p_actor_user_id,
            'project_id', project_id,
            'api_key_id', api_key_id,
            'membership_id', membership_id
        )
    );

    RETURN NEXT;
END;
$$;

DROP POLICY IF EXISTS tenants_isolation ON tenants;
CREATE POLICY tenants_isolation ON tenants
    FOR ALL
    USING (id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (
        id = nullif(current_setting('app.tenant_id', true), '')::bigint
        OR nullif(current_setting('app.allow_tenant_bootstrap', true), '') = '1'
    );

DROP POLICY IF EXISTS projects_isolation ON projects;
CREATE POLICY projects_isolation ON projects
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS api_keys_isolation ON api_keys;
CREATE POLICY api_keys_isolation ON api_keys
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS end_users_isolation ON end_users;
CREATE POLICY end_users_isolation ON end_users
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS sessions_isolation ON sessions;
CREATE POLICY sessions_isolation ON sessions
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS storage_objects_isolation ON storage_objects;
CREATE POLICY storage_objects_isolation ON storage_objects
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS leaderboards_isolation ON leaderboards;
CREATE POLICY leaderboards_isolation ON leaderboards
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS leaderboard_entries_isolation ON leaderboard_entries;
CREATE POLICY leaderboard_entries_isolation ON leaderboard_entries
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS friend_edges_isolation ON friend_edges;
CREATE POLICY friend_edges_isolation ON friend_edges
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS audit_log_isolation ON audit_log;
CREATE POLICY audit_log_isolation ON audit_log
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS usage_samples_isolation ON usage_samples;
CREATE POLICY usage_samples_isolation ON usage_samples
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS game_server_allocations_isolation ON game_server_allocations;
CREATE POLICY game_server_allocations_isolation ON game_server_allocations
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS matchmaking_tickets_isolation ON matchmaking_tickets;
CREATE POLICY matchmaking_tickets_isolation ON matchmaking_tickets
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS end_user_invitations_isolation ON end_user_invitations;
CREATE POLICY end_user_invitations_isolation ON end_user_invitations
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS fleet_allocation_events_isolation ON fleet_allocation_events;
CREATE POLICY fleet_allocation_events_isolation ON fleet_allocation_events
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS fleets_isolation ON fleets;
CREATE POLICY fleets_isolation ON fleets
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

ALTER TABLE feature_grants ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS feature_grants_isolation ON feature_grants;
CREATE POLICY feature_grants_isolation ON feature_grants
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
ALTER TABLE api_keys FORCE ROW LEVEL SECURITY;
ALTER TABLE end_users FORCE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
ALTER TABLE storage_objects FORCE ROW LEVEL SECURITY;
ALTER TABLE leaderboards FORCE ROW LEVEL SECURITY;
ALTER TABLE leaderboard_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE friend_edges FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;
ALTER TABLE usage_samples FORCE ROW LEVEL SECURITY;
ALTER TABLE game_server_allocations FORCE ROW LEVEL SECURITY;
ALTER TABLE matchmaking_tickets FORCE ROW LEVEL SECURITY;
ALTER TABLE end_user_invitations FORCE ROW LEVEL SECURITY;
ALTER TABLE fleet_allocation_events FORCE ROW LEVEL SECURITY;
ALTER TABLE fleets FORCE ROW LEVEL SECURITY;
ALTER TABLE feature_grants FORCE ROW LEVEL SECURITY;
