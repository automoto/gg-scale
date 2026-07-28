DELETE FROM casbin_rule
WHERE ptype = 'p'
  AND v0 = 'role:tenant_admin'
  AND v1 = '*'
  AND v2 = 'api_key:secret'
  AND v3 = 'manage';
