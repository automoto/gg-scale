DROP POLICY IF EXISTS usage_samples_isolation ON usage_samples;
ALTER TABLE usage_samples DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS audit_log_isolation ON audit_log;
ALTER TABLE audit_log DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS friend_edges_isolation ON friend_edges;
ALTER TABLE friend_edges DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS leaderboard_entries_isolation ON leaderboard_entries;
ALTER TABLE leaderboard_entries DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS leaderboards_isolation ON leaderboards;
ALTER TABLE leaderboards DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS storage_objects_isolation ON storage_objects;
ALTER TABLE storage_objects DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sessions_isolation ON sessions;
ALTER TABLE sessions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS end_users_isolation ON end_users;
ALTER TABLE end_users DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS api_keys_isolation ON api_keys;
ALTER TABLE api_keys DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS projects_isolation ON projects;
ALTER TABLE projects DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenants_isolation ON tenants;
ALTER TABLE tenants DISABLE ROW LEVEL SECURITY;
