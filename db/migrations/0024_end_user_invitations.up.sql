-- Player invitations: a tenant admin invites someone to join a project as
-- a game player (end_user). Acceptance creates the end_user row.

CREATE TABLE end_user_invitations (
    id                  BIGSERIAL PRIMARY KEY,
    tenant_id           BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id          BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    email               CITEXT NOT NULL,
    -- code_hash is SHA-256(code); see dashboard_invitations for rationale.
    code_hash           BYTEA NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    accepted_at         TIMESTAMPTZ,
    revoked_at          TIMESTAMPTZ,
    invited_by_user_id  BIGINT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX end_user_invitations_open_uq
    ON end_user_invitations (project_id, email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE INDEX end_user_invitations_tenant_idx
    ON end_user_invitations (tenant_id);

CREATE INDEX end_user_invitations_code_lookup_idx
    ON end_user_invitations (code_hash)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

ALTER TABLE end_user_invitations ENABLE ROW LEVEL SECURITY;
CREATE POLICY end_user_invitations_isolation ON end_user_invitations
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON end_user_invitations TO ggscale_app;
GRANT USAGE, SELECT ON end_user_invitations_id_seq TO ggscale_app;
