-- Dashboard operators are platform-scoped users. Tenant access is modeled
-- through memberships so one user can administer many tenants.

CREATE TABLE dashboard_users (
    id                  BIGSERIAL PRIMARY KEY,
    email               CITEXT NOT NULL UNIQUE,
    password_hash       BYTEA NOT NULL,
    is_platform_admin   BOOLEAN NOT NULL DEFAULT false,
    login_failures      INTEGER NOT NULL DEFAULT 0,
    locked_until        TIMESTAMPTZ,
    last_login_at       TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE dashboard_memberships (
    id                  BIGSERIAL PRIMARY KEY,
    dashboard_user_id   BIGINT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
    tenant_id           BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    role                TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dashboard_user_id, tenant_id)
);

CREATE INDEX dashboard_memberships_tenant_idx ON dashboard_memberships (tenant_id);

CREATE TABLE dashboard_sessions (
    id                  BIGSERIAL PRIMARY KEY,
    dashboard_user_id   BIGINT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
    refresh_hash        BYTEA NOT NULL UNIQUE,
    csrf_secret         BYTEA NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at          TIMESTAMPTZ,
    ip                  TEXT,
    user_agent          TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX dashboard_sessions_user_active_idx
    ON dashboard_sessions (dashboard_user_id)
    WHERE revoked_at IS NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON
    dashboard_users,
    dashboard_memberships,
    dashboard_sessions
TO ggscale_app;

GRANT USAGE, SELECT ON
    dashboard_users_id_seq,
    dashboard_memberships_id_seq,
    dashboard_sessions_id_seq
TO ggscale_app;

CREATE POLICY tenants_dashboard_membership ON tenants
    FOR SELECT
    USING (
        nullif(current_setting('app.dashboard_user_id', true), '') IS NOT NULL
        AND EXISTS (
            SELECT 1
            FROM dashboard_users u
            WHERE u.id = nullif(current_setting('app.dashboard_user_id', true), '')::bigint
              AND (
                  u.is_platform_admin
                  OR EXISTS (
                      SELECT 1
                      FROM dashboard_memberships m
                      WHERE m.dashboard_user_id = u.id
                        AND m.tenant_id = tenants.id
                  )
              )
        )
    );
