-- End users are the players authenticated by ggscale on behalf of a tenant's
-- game. external_id is the tenant's stable identifier (Steam ID, anonymous
-- UUID, etc.). Sessions hold opaque refresh tokens; only the hash is stored.

CREATE TABLE end_users (
    id                  BIGSERIAL PRIMARY KEY,
    tenant_id           BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id          BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    external_id         TEXT NOT NULL,
    email               CITEXT,
    email_verified_at   TIMESTAMPTZ,
    password_hash       BYTEA,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ
);

CREATE INDEX end_users_tenant_id_idx ON end_users (tenant_id);
CREATE UNIQUE INDEX end_users_external_uniq
    ON end_users (tenant_id, project_id, external_id)
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX end_users_email_uniq
    ON end_users (tenant_id, project_id, email)
    WHERE email IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE sessions (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    end_user_id     BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    refresh_hash    BYTEA NOT NULL UNIQUE,
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX sessions_tenant_id_idx ON sessions (tenant_id);
CREATE INDEX sessions_end_user_id_idx ON sessions (end_user_id);
CREATE INDEX sessions_active_idx ON sessions (tenant_id, expires_at) WHERE revoked_at IS NULL;
