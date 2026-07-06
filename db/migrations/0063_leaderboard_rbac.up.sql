INSERT INTO casbin_rule (ptype, v0, v1, v2, v3)
VALUES
    ('p', 'role:tenant_owner', '*', 'project:*:leaderboard', 'manage'),
    ('p', 'role:tenant_admin', '*', 'project:*:leaderboard', 'manage')
ON CONFLICT DO NOTHING;
