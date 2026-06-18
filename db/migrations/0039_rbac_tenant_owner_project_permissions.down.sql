DELETE FROM casbin_rule
WHERE ptype = 'p'
  AND v0 = 'role:tenant_owner'
  AND v1 = '*'
  AND v2 IN (
      'project:*:players',
      'project:*:config',
      'project:*:fleet',
      'project:*:allocation',
      'project:*:matchmaker',
      'project:*:relay',
      'project:*:matchmaking:dedicated'
  )
  AND v3 = '*';
