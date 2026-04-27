-- Row-Level Security defense-in-depth: every tenant-scoped table is filtered
-- against the per-transaction `app.tenant_id` GUC the application sets via
-- SET LOCAL inside db.Q(ctx). The application-level tenant middleware is the
-- primary boundary; RLS is the second wall.
--
-- `current_setting('app.tenant_id', true)` returns NULL when the GUC is
-- unset, so the policy fails closed (no rows visible) for any code path that
-- forgot to set it.

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenants_isolation ON tenants
    FOR ALL
    USING (id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
CREATE POLICY projects_isolation ON projects
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY api_keys_isolation ON api_keys
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE end_users ENABLE ROW LEVEL SECURITY;
CREATE POLICY end_users_isolation ON end_users
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
CREATE POLICY sessions_isolation ON sessions
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE storage_objects ENABLE ROW LEVEL SECURITY;
CREATE POLICY storage_objects_isolation ON storage_objects
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE leaderboards ENABLE ROW LEVEL SECURITY;
CREATE POLICY leaderboards_isolation ON leaderboards
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE leaderboard_entries ENABLE ROW LEVEL SECURITY;
CREATE POLICY leaderboard_entries_isolation ON leaderboard_entries
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE friend_edges ENABLE ROW LEVEL SECURITY;
CREATE POLICY friend_edges_isolation ON friend_edges
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY audit_log_isolation ON audit_log
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

ALTER TABLE usage_samples ENABLE ROW LEVEL SECURITY;
CREATE POLICY usage_samples_isolation ON usage_samples
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);
