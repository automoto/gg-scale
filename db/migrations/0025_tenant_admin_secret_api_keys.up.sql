-- Tenant admins can create and manage secret API keys, mirroring
-- rbac.defaultPolicyCSV. Previously secret keys were owner-only, which left
-- invited tenant admins unable to issue the server-side key their game needs.
INSERT INTO casbin_rule (ptype, v0, v1, v2, v3) VALUES
    ('p', 'role:tenant_admin', '*', 'api_key:secret', 'manage')
ON CONFLICT DO NOTHING;
