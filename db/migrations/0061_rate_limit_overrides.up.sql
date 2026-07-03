-- Configurable per-tenant / per-project rate-limit overrides.
--
-- Two audiences write here:
--   * Platform admins set the tenant-level HTTP API limit (kind='api',
--     project_id NULL) — overriding the compiled tier defaults in
--     internal/ratelimit/tier.go.
--   * Tenant admins set per-project invite quotas (kind='invite_*') — clamped
--     to tenant-level values in the application layer.
--
-- Each row is one token-bucket (rate tokens/sec, burst capacity), so a wide
-- range of limits maps onto the same two columns:
--   10 invites/hour  -> burst=10,  rate=10/3600
--   1 per 10 min     -> burst=1,   rate=1/600
--
-- No RLS: like feature_grants, this is read in mixed transaction contexts
-- (tenant Pool.Q and BootstrapQ) so every query filters tenant_id explicitly
-- rather than relying on app.tenant_id.
CREATE TABLE rate_limit_overrides (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id BIGINT REFERENCES projects(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN (
        'api',
        'invite_inviter',
        'invite_domain',
        'invite_recipient'
    )),
    rate       DOUBLE PRECISION NOT NULL CHECK (rate >= 0),
    burst      DOUBLE PRECISION NOT NULL CHECK (burst >= 0),
    updated_by BIGINT REFERENCES dashboard_users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One override per (tenant, project-or-tenant-wide, kind). COALESCE(project_id,
-- 0) collapses the NULL (tenant-wide) case into the unique key, matching the
-- feature_grants convention.
CREATE UNIQUE INDEX rate_limit_overrides_unique_idx
    ON rate_limit_overrides (tenant_id, COALESCE(project_id, 0), kind);

CREATE INDEX rate_limit_overrides_tenant_kind_idx
    ON rate_limit_overrides (tenant_id, kind);

GRANT SELECT, INSERT, UPDATE, DELETE ON rate_limit_overrides TO ggscale_app;
GRANT USAGE, SELECT ON rate_limit_overrides_id_seq TO ggscale_app;
