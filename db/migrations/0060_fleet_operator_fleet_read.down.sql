DELETE FROM casbin_rule
WHERE ptype = 'p'
  AND v0 = 'role:fleet_operator'
  AND v1 = '*'
  AND v2 = 'project:*:fleet'
  AND v3 = 'read';
