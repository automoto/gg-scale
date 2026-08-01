-- Server-tier storage: secret API keys (role:api_server) may read/write/list a
-- player's storage objects on the /v1/server/ routes. Mirrors the
-- rbac.defaultPolicyCSV row added in the same change; the casbin-parity guard
-- test enforces the match.
INSERT INTO casbin_rule (ptype, v0, v1, v2, v3)
VALUES ('p', 'role:api_server', '*', 'storage', 'manage')
ON CONFLICT DO NOTHING;
