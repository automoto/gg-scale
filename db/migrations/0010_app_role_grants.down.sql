DROP POLICY IF EXISTS api_keys_bootstrap ON api_keys;

ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE, SELECT ON SEQUENCES FROM ggscale_app;
REVOKE USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public FROM ggscale_app;

REVOKE SELECT, INSERT, UPDATE, DELETE ON
    tenants, projects, api_keys,
    end_users, sessions,
    storage_objects,
    leaderboards, leaderboard_entries,
    friend_edges,
    usage_samples
FROM ggscale_app;
