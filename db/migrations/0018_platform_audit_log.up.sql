CREATE TABLE platform_audit_log (
    id            BIGSERIAL PRIMARY KEY,
    actor_user_id BIGINT,
    action        TEXT NOT NULL,
    target        TEXT,
    payload       JSONB NOT NULL DEFAULT '{}',
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX platform_audit_log_action_idx ON platform_audit_log (action, occurred_at DESC);

REVOKE UPDATE, DELETE ON platform_audit_log FROM ggscale_app;
GRANT INSERT, SELECT ON platform_audit_log TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE platform_audit_log_id_seq TO ggscale_app;
