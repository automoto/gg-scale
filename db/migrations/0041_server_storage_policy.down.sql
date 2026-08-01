DELETE FROM casbin_rule
WHERE ptype = 'p'
  AND v0 = 'role:api_server'
  AND v1 = '*'
  AND v2 = 'storage'
  AND v3 = 'manage';
