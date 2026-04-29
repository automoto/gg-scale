-- Once a connection has run SET LOCAL app.tenant_id inside a transaction,
-- subsequent calls to current_setting('app.tenant_id', true) on the same
-- connection (after commit/rollback) can return '' instead of NULL.
-- Casting '' to bigint then ERRORs (22P02), which surfaces as a 500 in
-- the tenant middleware's bootstrap api_keys SELECT.
--
-- Wrap every current_setting reference in nullif(..., '') so '' and NULL
-- are equivalent. Defensive: the bootstrap policies already test for both
-- but the isolation policies didn't.

DROP POLICY IF EXISTS tenants_isolation ON tenants;
CREATE POLICY tenants_isolation ON tenants
    FOR ALL
    USING (id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS projects_isolation ON projects;
CREATE POLICY projects_isolation ON projects
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS api_keys_isolation ON api_keys;
CREATE POLICY api_keys_isolation ON api_keys
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS end_users_isolation ON end_users;
CREATE POLICY end_users_isolation ON end_users
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS sessions_isolation ON sessions;
CREATE POLICY sessions_isolation ON sessions
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS storage_objects_isolation ON storage_objects;
CREATE POLICY storage_objects_isolation ON storage_objects
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS leaderboards_isolation ON leaderboards;
CREATE POLICY leaderboards_isolation ON leaderboards
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS leaderboard_entries_isolation ON leaderboard_entries;
CREATE POLICY leaderboard_entries_isolation ON leaderboard_entries
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS friend_edges_isolation ON friend_edges;
CREATE POLICY friend_edges_isolation ON friend_edges
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS audit_log_isolation ON audit_log;
CREATE POLICY audit_log_isolation ON audit_log
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

DROP POLICY IF EXISTS usage_samples_isolation ON usage_samples;
CREATE POLICY usage_samples_isolation ON usage_samples
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);
