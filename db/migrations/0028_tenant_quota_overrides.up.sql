-- Per-tenant quota-axis overrides layered over the per-class ladder
-- (internal/quota.Resolve). Written only by platform admins — directly or by
-- approving a tenant change request. Like storage_limits, platform-global
-- with explicit tenant filtering (no RLS); the hot paths read it aggregated
-- into the tenant quota-context queries.
CREATE TABLE tenant_quota_overrides (
    tenant_id  BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    axis       TEXT   NOT NULL CHECK (axis IN ('projects', 'players', 'storage', 'relay_sessions', 'open_sessions')),
    -- -1 is the quota.Unlimited sentinel; 0 blocks all new growth on the axis.
    "limit"    BIGINT NOT NULL CHECK ("limit" >= -1),
    updated_by BIGINT REFERENCES control_panel_users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, axis)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_quota_overrides TO ggscale_app;
