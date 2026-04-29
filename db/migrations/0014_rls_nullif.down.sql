-- Restore the original isolation policies (without nullif). Mirrors 0009.

DROP POLICY IF EXISTS usage_samples_isolation ON usage_samples;
CREATE POLICY usage_samples_isolation ON usage_samples
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS audit_log_isolation ON audit_log;
CREATE POLICY audit_log_isolation ON audit_log
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS friend_edges_isolation ON friend_edges;
CREATE POLICY friend_edges_isolation ON friend_edges
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS leaderboard_entries_isolation ON leaderboard_entries;
CREATE POLICY leaderboard_entries_isolation ON leaderboard_entries
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS leaderboards_isolation ON leaderboards;
CREATE POLICY leaderboards_isolation ON leaderboards
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS storage_objects_isolation ON storage_objects;
CREATE POLICY storage_objects_isolation ON storage_objects
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS sessions_isolation ON sessions;
CREATE POLICY sessions_isolation ON sessions
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS end_users_isolation ON end_users;
CREATE POLICY end_users_isolation ON end_users
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS api_keys_isolation ON api_keys;
CREATE POLICY api_keys_isolation ON api_keys
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS projects_isolation ON projects;
CREATE POLICY projects_isolation ON projects
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

DROP POLICY IF EXISTS tenants_isolation ON tenants;
CREATE POLICY tenants_isolation ON tenants
    FOR ALL
    USING (id = current_setting('app.tenant_id', true)::bigint);
