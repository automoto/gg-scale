-- Platform-admin tenant-management policy, mirroring rbac.defaultPolicyCSV.
-- Replaces the code-level platform-admin bypass in CanControlPanel: platform
-- admins now get tenant-scope capability from these p-rules plus their
-- "*"-domain grouping row.
INSERT INTO casbin_rule (ptype, v0, v1, v2, v3) VALUES
    ('p', 'role:platform_admin', '*', 'tenant', 'manage'),
    ('p', 'role:platform_admin', '*', 'project', 'manage'),
    ('p', 'role:platform_admin', '*', 'project', 'read'),
    ('p', 'role:platform_admin', '*', 'project:*:players', '*'),
    ('p', 'role:platform_admin', '*', 'project:*:config', '*'),
    ('p', 'role:platform_admin', '*', 'project:*:fleet', '*'),
    ('p', 'role:platform_admin', '*', 'project:*:allocation', '*'),
    ('p', 'role:platform_admin', '*', 'project:*:matchmaker', '*'),
    ('p', 'role:platform_admin', '*', 'project:*:relay', '*'),
    ('p', 'role:platform_admin', '*', 'project:*:matchmaking:dedicated', '*'),
    ('p', 'role:platform_admin', '*', 'project:*:leaderboard', 'manage'),
    ('p', 'role:platform_admin', '*', 'api_key:*', 'manage'),
    ('p', 'role:platform_admin', '*', 'team', 'manage'),
    ('p', 'role:platform_admin', '*', 'audit', 'read')
ON CONFLICT DO NOTHING;

-- Backfill grouping rows for platform admins created before the g-row was
-- written at creation time (the bootstrap first-admin path predates it).
INSERT INTO casbin_rule (ptype, v0, v1, v2)
SELECT 'g', 'control_panel:user:' || id, 'role:platform_admin', '*'
FROM control_panel_users
WHERE is_platform_admin
ON CONFLICT DO NOTHING;
