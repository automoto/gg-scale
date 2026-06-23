-- Player presence: online status and optional current session. One row
-- per (tenant, end_user) pair; upserted on every PUT /v1/presence call.
-- status is a free-form short string (1–32 chars) so any game can define
-- its own presence vocabulary, not just a fixed console-specific enum.

CREATE TABLE presence (
    tenant_id   BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    end_user_id BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    status      TEXT   NOT NULL DEFAULT 'online'
                CHECK (char_length(status) BETWEEN 1 AND 32),
    session_id  TEXT,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, end_user_id)
);

ALTER TABLE presence ENABLE ROW LEVEL SECURITY;
ALTER TABLE presence FORCE ROW LEVEL SECURITY;
CREATE POLICY presence_isolation ON presence
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON presence TO ggscale_app;
