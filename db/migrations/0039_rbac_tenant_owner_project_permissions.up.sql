INSERT INTO casbin_rule (ptype, v0, v1, v2, v3)
VALUES
    ('p', 'role:tenant_owner', '*', 'project:*:players', '*'),
    ('p', 'role:tenant_owner', '*', 'project:*:config', '*'),
    ('p', 'role:tenant_owner', '*', 'project:*:fleet', '*'),
    ('p', 'role:tenant_owner', '*', 'project:*:allocation', '*'),
    ('p', 'role:tenant_owner', '*', 'project:*:matchmaker', '*'),
    ('p', 'role:tenant_owner', '*', 'project:*:relay', '*'),
    ('p', 'role:tenant_owner', '*', 'project:*:matchmaking:dedicated', '*')
ON CONFLICT DO NOTHING;
