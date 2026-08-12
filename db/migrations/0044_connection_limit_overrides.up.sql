-- Per-tenant realtime admission envelopes let operators prepare a breakout
-- title or a contracted launch without lifting the compiled default for every
-- tenant in the same billing class. This is platform configuration, not
-- tenant-owned data, so it is accessed only through BootstrapQ and has no RLS.
CREATE TABLE connection_limit_overrides (
    tenant_id bigint PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    sustained bigint NOT NULL CHECK (sustained > 0),
    ceiling bigint NOT NULL CHECK (ceiling >= sustained),
    updated_by bigint REFERENCES control_panel_users(id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, DELETE, UPDATE ON connection_limit_overrides TO ggscale_app;
