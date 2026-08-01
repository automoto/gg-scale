DELETE FROM casbin_rule
WHERE ptype = 'p'
  AND v0 IN ('role:platform_admin', 'role:tenant_owner', 'role:tenant_admin')
  AND v1 = '*'
  AND v2 = 'custom_token'
  AND v3 = 'manage';

ALTER TABLE tenants
ADD COLUMN custom_token_secret bytea,
DROP COLUMN custom_token_public_key;
