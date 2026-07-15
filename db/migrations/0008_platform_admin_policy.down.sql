-- Remove the platform-admin tenant-management p-rules added by 0008. The
-- backfilled grouping rows stay: they are indistinguishable from runtime
-- grants and are inert under the pre-0008 binary.
DELETE FROM casbin_rule
WHERE ptype = 'p'
  AND v0 = 'role:platform_admin'
  AND v1 = '*'
  AND (v2, v3) IN (
    ('tenant', 'manage'),
    ('project', 'manage'),
    ('project', 'read'),
    ('project:*:players', '*'),
    ('project:*:config', '*'),
    ('project:*:fleet', '*'),
    ('project:*:allocation', '*'),
    ('project:*:matchmaker', '*'),
    ('project:*:relay', '*'),
    ('project:*:matchmaking:dedicated', '*'),
    ('project:*:leaderboard', 'manage'),
    ('api_key:*', 'manage'),
    ('team', 'manage'),
    ('audit', 'read')
  );
