CREATE TABLE casbin_rule (
    id BIGSERIAL PRIMARY KEY,
    ptype TEXT NOT NULL,
    v0 TEXT,
    v1 TEXT,
    v2 TEXT,
    v3 TEXT,
    v4 TEXT,
    v5 TEXT
);

CREATE UNIQUE INDEX casbin_rule_unique_idx
    ON casbin_rule (
        ptype,
        COALESCE(v0, ''),
        COALESCE(v1, ''),
        COALESCE(v2, ''),
        COALESCE(v3, ''),
        COALESCE(v4, ''),
        COALESCE(v5, '')
    );

CREATE TABLE feature_grants (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id BIGINT REFERENCES projects(id) ON DELETE CASCADE,
    feature TEXT NOT NULL CHECK (feature IN (
        'p2p_relay',
        'dedicated_servers',
        'fleet_docker_backend',
        'fleet_agones_backend',
        'fleet_plugin_backend'
    )),
    enabled BOOLEAN NOT NULL DEFAULT false,
    approved_by_dashboard_user_id BIGINT REFERENCES dashboard_users(id) ON DELETE SET NULL,
    reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX feature_grants_unique_idx
    ON feature_grants (tenant_id, COALESCE(project_id, 0), feature);

CREATE INDEX feature_grants_tenant_feature_idx
    ON feature_grants (tenant_id, feature);

INSERT INTO casbin_rule (ptype, v0, v1, v2, v3)
VALUES
    ('p', 'role:platform_owner', '*', '*', '*'),
    ('p', 'role:platform_admin', '*', 'tenant', 'read'),
    ('p', 'role:platform_admin', '*', 'dashboard_user', 'read'),
    ('p', 'role:platform_admin', '*', 'dashboard_user', 'disable'),
    ('p', 'role:platform_admin', '*', 'platform:plugins', 'read'),
    ('p', 'role:platform_support', '*', 'tenant', 'read'),
    ('p', 'role:platform_support', '*', 'dashboard_user', 'read'),
    ('p', 'role:tenant_owner', '*', 'tenant', 'manage'),
    ('p', 'role:tenant_owner', '*', 'project', 'manage'),
    ('p', 'role:tenant_owner', '*', 'api_key', 'manage'),
    ('p', 'role:tenant_owner', '*', 'team', 'manage'),
    ('p', 'role:tenant_owner', '*', 'audit', 'read'),
    ('p', 'role:tenant_admin', '*', 'project', 'manage'),
    ('p', 'role:tenant_admin', '*', 'project:*:players', 'manage'),
    ('p', 'role:tenant_admin', '*', 'api_key:publishable', 'manage'),
    ('p', 'role:tenant_admin', '*', 'audit', 'read'),
    ('p', 'role:security_admin', '*', 'api_key:secret', 'manage'),
    ('p', 'role:security_admin', '*', 'custom_token', 'manage'),
    ('p', 'role:security_admin', '*', 'audit', 'read'),
    ('p', 'role:security_admin', '*', 'feature_request', 'create'),
    ('p', 'role:developer', '*', 'project', 'read'),
    ('p', 'role:developer', '*', 'project:*:config', 'update'),
    ('p', 'role:developer', '*', 'project:*:players', 'read'),
    ('p', 'role:support', '*', 'project:*:players', 'read'),
    ('p', 'role:support', '*', 'project:*:players', 'invite'),
    ('p', 'role:support', '*', 'project:*:players', 'disable'),
    ('p', 'role:analyst', '*', 'project', 'read'),
    ('p', 'role:analyst', '*', 'project:*:players', 'read'),
    ('p', 'role:analyst', '*', 'project:*:allocation', 'read'),
    ('p', 'role:analyst', '*', 'project:*:matchmaker', 'read'),
    ('p', 'role:fleet_operator', '*', 'project:*:fleet', 'manage'),
    ('p', 'role:fleet_operator', '*', 'project:*:allocation', 'read'),
    ('p', 'role:fleet_operator', '*', 'project:*:allocation', 'allocate'),
    ('p', 'role:fleet_operator', '*', 'project:*:allocation', 'deallocate'),
    ('p', 'role:fleet_operator', '*', 'project:*:matchmaker', 'read'),
    ('p', 'role:player_standard', '*', 'profile', 'read'),
    ('p', 'role:player_standard', '*', 'profile', 'update'),
    ('p', 'role:player_standard', '*', 'storage', 'manage'),
    ('p', 'role:player_standard', '*', 'friends', 'manage'),
    ('p', 'role:player_standard', '*', 'leaderboard', 'read'),
    ('p', 'role:player_standard', '*', 'realtime', 'connect'),
    ('p', 'role:player_verified', '*', 'profile', 'read'),
    ('p', 'role:player_verified', '*', 'profile', 'update'),
    ('p', 'role:player_verified', '*', 'storage', 'manage'),
    ('p', 'role:player_verified', '*', 'friends', 'manage'),
    ('p', 'role:player_verified', '*', 'leaderboard', 'read'),
    ('p', 'role:player_verified', '*', 'realtime', 'connect'),
    ('p', 'role:player_high_access', '*', 'project:*:relay', 'issue_credentials'),
    ('p', 'role:player_high_access', '*', 'project:*:matchmaking:dedicated', 'create_ticket'),
    ('p', 'role:api_client', '*', 'auth', 'create'),
    ('p', 'role:api_client', '*', 'profile', 'read'),
    ('p', 'role:api_server', '*', 'end_user', 'verify'),
    ('p', 'role:api_server', '*', 'leaderboard', 'submit'),
    ('p', 'role:api_fleet_runtime', '*', 'end_user', 'verify'),
    ('p', 'role:api_fleet_runtime', '*', 'project:*:allocation', 'read'),
    ('p', 'role:api_fleet_runtime', '*', 'project:*:allocation', 'update')
ON CONFLICT DO NOTHING;

INSERT INTO casbin_rule (ptype, v0, v1, v2)
SELECT
    'g',
    'dashboard:user:' || id::TEXT,
    'role:platform_admin',
    '*'
FROM dashboard_users
WHERE is_platform_admin = true
ON CONFLICT DO NOTHING;

INSERT INTO casbin_rule (ptype, v0, v1, v2)
SELECT
    'g',
    'dashboard:user:' || dashboard_user_id::TEXT,
    CASE role
        WHEN 'owner' THEN 'role:tenant_owner'
        WHEN 'admin' THEN 'role:tenant_admin'
        WHEN 'member' THEN 'role:analyst'
    END,
    'tenant:' || tenant_id::TEXT
FROM dashboard_memberships
WHERE role IN ('owner', 'admin', 'member')
ON CONFLICT DO NOTHING;

INSERT INTO casbin_rule (ptype, v0, v1, v2)
SELECT
    'g',
    'api_key:' || id::TEXT,
    CASE COALESCE(key_type, 'secret')
        WHEN 'publishable' THEN 'role:api_client'
        WHEN 'secret' THEN 'role:api_server'
    END,
    'tenant:' || tenant_id::TEXT
FROM api_keys
WHERE revoked_at IS NULL
  AND COALESCE(key_type, 'secret') IN ('publishable', 'secret')
ON CONFLICT DO NOTHING;
