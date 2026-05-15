-- Dashboard invitations: a platform admin or tenant admin pre-creates a
-- pending membership tied to an email + role. Acceptance creates (or links
-- to) a dashboard_user and writes the membership row.
--
-- tenant_id NULL means a platform_admin invite (no tenant scope).
-- role = 'platform_admin' | 'tenant_admin' | 'tenant_member'.
-- code_hash is SHA-256(salt || code); salt prevents rainbow tables on the
-- 26^N magic-link code space.

CREATE TABLE dashboard_invitations (
    id                  BIGSERIAL PRIMARY KEY,
    email               CITEXT NOT NULL,
    tenant_id           BIGINT REFERENCES tenants(id) ON DELETE CASCADE,
    role                TEXT NOT NULL CHECK (role IN ('platform_admin', 'tenant_admin', 'tenant_member')),
    -- code_hash is SHA-256(code). Codes are 24 random bytes (192 bits),
    -- so per-row salt would be pointless — we keep the hash unsalted so
    -- the magic-link landing page can look the invite up by code alone.
    code_hash           BYTEA NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    accepted_at         TIMESTAMPTZ,
    revoked_at          TIMESTAMPTZ,
    invited_by_user_id  BIGINT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A given email can have at most one open invite per tenant scope. (NULL
-- tenant_id is a distinct scope handled by COALESCE.)
CREATE UNIQUE INDEX dashboard_invitations_open_uq
    ON dashboard_invitations (email, COALESCE(tenant_id, 0))
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE INDEX dashboard_invitations_tenant_idx
    ON dashboard_invitations (tenant_id)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE INDEX dashboard_invitations_code_lookup_idx
    ON dashboard_invitations (code_hash)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON dashboard_invitations TO ggscale_app;
GRANT USAGE, SELECT ON dashboard_invitations_id_seq TO ggscale_app;
