-- Regional realtime connection capacity is leased to each application process
-- in blocks. WebSocket admission consumes those blocks from process memory;
-- PostgreSQL is only touched to allocate/renew/release a block.
CREATE TABLE realtime_connection_cap_states (
    tenant_id          BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    region             TEXT NOT NULL CHECK (region <> ''),
    sustained          BIGINT NOT NULL CHECK (sustained > 0),
    ceiling            BIGINT NOT NULL CHECK (ceiling >= sustained),
    burst_remaining_ns BIGINT NOT NULL CHECK (burst_remaining_ns >= 0),
    last_assessed_at   TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, region)
);

CREATE TABLE realtime_connection_grants (
    tenant_id  BIGINT NOT NULL,
    region     TEXT NOT NULL,
    holder_id  TEXT NOT NULL CHECK (holder_id <> ''),
    allocated  BIGINT NOT NULL CHECK (allocated >= 0),
    used       BIGINT NOT NULL CHECK (used >= 0),
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, region, holder_id),
    FOREIGN KEY (tenant_id, region)
        REFERENCES realtime_connection_cap_states (tenant_id, region)
        ON DELETE CASCADE
);

CREATE INDEX realtime_connection_grants_expiry_idx
    ON realtime_connection_grants (expires_at);

ALTER TABLE realtime_connection_cap_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE realtime_connection_cap_states FORCE ROW LEVEL SECURITY;
CREATE POLICY realtime_connection_cap_states_isolation
    ON realtime_connection_cap_states
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

ALTER TABLE realtime_connection_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE realtime_connection_grants FORCE ROW LEVEL SECURITY;
CREATE POLICY realtime_connection_grants_isolation
    ON realtime_connection_grants
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

-- The process renewal worker updates all of its own tenant grants in one batch
-- so cross-region database RTT is paid once per interval, not once per tenant.
-- The query is constrained by the unguessable process boot ID and region.
-- UPDATE ... RETURNING also requires SELECT visibility under RLS.
CREATE POLICY realtime_connection_grants_worker_select
    ON realtime_connection_grants
    FOR SELECT
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NULL);

CREATE POLICY realtime_connection_grants_worker_update
    ON realtime_connection_grants
    FOR UPDATE
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    WITH CHECK (NULLIF(current_setting('app.tenant_id', true), '') IS NULL);

-- The periodic GC worker removes expired grants for processes that disappeared
-- before releasing their rows. It runs without a request tenant.
CREATE POLICY realtime_connection_grants_worker_delete
    ON realtime_connection_grants
    FOR DELETE
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NULL);

GRANT SELECT, INSERT, UPDATE, DELETE ON realtime_connection_cap_states TO ggscale_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON realtime_connection_grants TO ggscale_app;
