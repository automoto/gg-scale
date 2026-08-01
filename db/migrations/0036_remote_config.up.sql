ALTER TABLE projects
ADD COLUMN remote_config jsonb NOT NULL DEFAULT '{}'::jsonb,
ADD CONSTRAINT projects_remote_config_object_chk
    CHECK (jsonb_typeof(remote_config) = 'object'),
-- The editor caps canonical input at 64 KiB. Leave headroom for PostgreSQL's
-- jsonb text formatting while still bounding rows written outside the editor.
ADD CONSTRAINT projects_remote_config_size_chk
    CHECK (octet_length(remote_config::text) <= 1048576);

INSERT INTO casbin_rule (ptype, v0, v1, v2, v3)
VALUES ('p', 'role:tenant_admin', '*', 'project:*:config', 'update')
ON CONFLICT DO NOTHING;
