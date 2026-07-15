-- Tenant-initiated change requests: tier upgrades and feature grants, reviewed
-- by a platform admin (docs/temp/tier-rework.md M5). Mirrors the tenant-signup
-- request flow. Platform-global (no RLS): the platform-admin queue reads it
-- cross-tenant and the tenant side filters by its path tenant_id explicitly.
CREATE TABLE tenant_change_requests (
    id                   BIGSERIAL PRIMARY KEY,
    tenant_id            BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    requested_by_user_id BIGINT REFERENCES control_panel_users(id) ON DELETE SET NULL,
    kind                 TEXT NOT NULL CHECK (kind IN ('tier_upgrade', 'feature')),
    requested_tier       SMALLINT CHECK (requested_tier BETWEEN 0 AND 3),
    feature              TEXT CHECK (feature = ANY (ARRAY[
                             'p2p_relay', 'dedicated_servers', 'fleet_docker_backend',
                             'fleet_agones_backend', 'fleet_plugin_backend', 'matchmaker'])),
    note                 TEXT NOT NULL DEFAULT '',
    status               TEXT NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'approved', 'denied')),
    reviewed_by_user_id  BIGINT REFERENCES control_panel_users(id) ON DELETE SET NULL,
    reviewed_at          TIMESTAMPTZ,
    review_reason        TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Shape: a tier_upgrade carries a target class and no feature; a feature
    -- request carries a feature and no class.
    CONSTRAINT tenant_change_requests_shape CHECK (
        (kind = 'tier_upgrade' AND requested_tier IS NOT NULL AND feature IS NULL) OR
        (kind = 'feature'      AND feature IS NOT NULL AND requested_tier IS NULL)
    )
);

-- At most one open request per (tenant, kind, feature): a tenant can't spam the
-- queue, and re-requesting the same thing while one is pending is rejected.
CREATE UNIQUE INDEX tenant_change_requests_one_pending
    ON tenant_change_requests (tenant_id, kind, COALESCE(feature, ''))
    WHERE status = 'pending';

CREATE INDEX tenant_change_requests_tenant_idx
    ON tenant_change_requests (tenant_id, created_at DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_change_requests TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE tenant_change_requests_id_seq TO ggscale_app;
