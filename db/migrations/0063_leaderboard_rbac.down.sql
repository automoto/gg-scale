DELETE FROM casbin_rule
WHERE ptype = 'p'
  AND v1 = '*'
  AND v2 = 'project:*:leaderboard'
  AND v3 = 'manage'
  AND v0 IN ('role:tenant_owner', 'role:tenant_admin');
