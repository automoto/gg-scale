-- Per-tenant realtime admission envelopes let operators prepare a breakout
-- title or a contracted launch without lifting the compiled default for every
-- tenant in the same billing class. This is platform configuration, not
-- tenant-owned data, so it is accessed only through BootstrapQ and has no RLS.
-- Keep it separate from rate_limit_overrides: that table models float token
-- buckets with optional project scope, while connection admission needs exact
-- integer counts, one row per tenant, and envelope-specific constraints.
CREATE TABLE connection_limit_overrides (
    tenant_id bigint PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    sustained bigint NOT NULL CHECK (sustained > 0 AND sustained <= 500000),
    ceiling bigint NOT NULL CHECK (
        ceiling >= sustained AND ceiling <= sustained * 2 AND ceiling <= 500000
    ),
    updated_by bigint REFERENCES control_panel_users(id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, DELETE, UPDATE ON connection_limit_overrides TO ggscale_app;
