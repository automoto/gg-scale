DELETE FROM casbin_rule
WHERE ptype = 'p'
  AND v0 = 'role:tenant_admin'
  AND v1 = '*'
  AND v2 = 'project:*:config'
  AND v3 = 'update';

ALTER TABLE projects
DROP CONSTRAINT projects_remote_config_size_chk,
DROP CONSTRAINT projects_remote_config_object_chk,
DROP COLUMN remote_config;
