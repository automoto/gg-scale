-- MCP tokens. A token is its own principal for one project, pasted into a
-- coding agent as a Bearer header. created_by_user_id is an upper limit on
-- what the token can do, never a source of rights. Only the SHA-256 of the
-- token is stored.
CREATE TABLE mcp_tokens (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id          BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id         BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_by_user_id BIGINT NOT NULL REFERENCES control_panel_users(id) ON DELETE CASCADE,
    label              TEXT NOT NULL CHECK (char_length(label) BETWEEN 1 AND 64),
    token_hash         BYTEA NOT NULL,
    token_hint         TEXT NOT NULL,
    scopes             TEXT[] NOT NULL DEFAULT '{}',
    expires_at         TIMESTAMPTZ NOT NULL,
    last_used_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at         TIMESTAMPTZ
);

CREATE UNIQUE INDEX mcp_tokens_hash_uniq ON mcp_tokens (token_hash);
CREATE INDEX mcp_tokens_project_idx ON mcp_tokens (tenant_id, project_id);

ALTER TABLE mcp_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_tokens FORCE ROW LEVEL SECURITY;
-- The request lookup runs before the tenant is known, the same as api_keys.
CREATE POLICY mcp_tokens_bootstrap ON mcp_tokens FOR SELECT
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NULL);
CREATE POLICY mcp_tokens_isolation ON mcp_tokens
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE ON mcp_tokens TO ggscale_app;

ALTER TABLE settings_revisions
    ADD CONSTRAINT settings_revisions_mcp_token_fk
    FOREIGN KEY (mcp_token_id) REFERENCES mcp_tokens(id) ON DELETE SET NULL;

-- Tenant admins can read the matchmaker queue, mirroring rbac.defaultPolicyCSV.
-- A token that an admin creates needs it for the ticket trace tool.
INSERT INTO casbin_rule (ptype, v0, v1, v2, v3) VALUES
    ('p', 'role:tenant_admin', '*', 'project:*:matchmaker', 'read')
ON CONFLICT DO NOTHING;
