DELETE FROM casbin_rule
WHERE ptype = 'p'
  AND v0 = 'role:tenant_admin'
  AND v1 = '*'
  AND v2 = 'project:*:matchmaker'
  AND v3 = 'read';

ALTER TABLE settings_revisions DROP CONSTRAINT settings_revisions_mcp_token_fk;
DROP TABLE mcp_tokens;
