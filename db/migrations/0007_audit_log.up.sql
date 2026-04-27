-- Append-only audit log. The dedicated ggscale_app role gets INSERT+SELECT
-- only; UPDATE/DELETE are revoked so a compromised app session cannot
-- rewrite history. The role itself is created NOLOGIN here; deployments
-- bind the app's connection user to it via SET ROLE / GRANT.

CREATE TABLE audit_log (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    actor_user_id   BIGINT REFERENCES end_users(id) ON DELETE SET NULL,
    action          TEXT NOT NULL,
    target          TEXT,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_tenant_id_idx ON audit_log (tenant_id, occurred_at DESC);
CREATE INDEX audit_log_actor_idx ON audit_log (tenant_id, actor_user_id, occurred_at DESC)
    WHERE actor_user_id IS NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ggscale_app') THEN
        CREATE ROLE ggscale_app NOLOGIN;
    END IF;
END$$;

REVOKE UPDATE, DELETE ON audit_log FROM ggscale_app;
GRANT INSERT, SELECT ON audit_log TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE audit_log_id_seq TO ggscale_app;
