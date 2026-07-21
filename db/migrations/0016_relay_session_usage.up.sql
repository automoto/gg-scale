-- Managed-relay session metering: one row per tenant per calendar month,
-- incremented at credential issuance. Backs the RelaySessionsPerMonth quota
-- axis: warn at 80%/100% of the class allowance, refuse only NEW issuance
-- past it — in-flight TURN sessions are never dropped.
CREATE TABLE relay_session_usage (
    tenant_id     BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    month         DATE NOT NULL,
    sessions      BIGINT NOT NULL DEFAULT 0 CHECK (sessions >= 0),
    warned_80_at  TIMESTAMPTZ,
    warned_100_at TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, month)
);

ALTER TABLE relay_session_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE relay_session_usage FORCE ROW LEVEL SECURITY;
CREATE POLICY relay_session_usage_isolation
    ON relay_session_usage
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE ON relay_session_usage TO ggscale_app;
