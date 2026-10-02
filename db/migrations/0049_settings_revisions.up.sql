-- Revision history for project settings written from the dashboard or by an
-- MCP token. The Go write path inserts a row in the same transaction as the
-- change and keeps the newest 3 rows for each resource. resource_id is the
-- project ID for remote config and the leaderboard ID for a leaderboard.
-- mcp_token_id has no FK yet: the token table comes in a later migration.
CREATE TABLE settings_revisions (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id     BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id    BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    resource_kind TEXT NOT NULL CHECK (resource_kind IN ('remote_config', 'leaderboard')),
    resource_id   BIGINT NOT NULL,
    revision      BIGINT NOT NULL CHECK (revision > 0),
    snapshot      JSONB NOT NULL,
    actor_user_id BIGINT REFERENCES control_panel_users(id) ON DELETE SET NULL,
    mcp_token_id  BIGINT,
    source        TEXT NOT NULL CHECK (source IN ('dashboard', 'mcp', 'rollback')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, resource_kind, resource_id, revision)
);

ALTER TABLE settings_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE settings_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY settings_revisions_isolation ON settings_revisions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, DELETE ON settings_revisions TO ggscale_app;
