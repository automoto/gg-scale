-- Per-tenant / per-project overrides for the maximum storage-object value size.
-- The platform default lives in config (STORAGE_MAX_VALUE_BYTES); a row here
-- overrides it, with the per-project row winning over the per-tenant row. Like
-- rate_limit_overrides and feature_grants, this table is platform-global with
-- explicit tenant filtering (no RLS) and read via BootstrapQ.
CREATE TABLE storage_limits (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id      BIGINT REFERENCES projects(id) ON DELETE CASCADE,
    max_value_bytes BIGINT NOT NULL CHECK (max_value_bytes > 0),
    updated_by      BIGINT REFERENCES control_panel_users(id) ON DELETE SET NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One override per (tenant, project); the tenant-level row uses a NULL project.
CREATE UNIQUE INDEX storage_limits_unique_idx
    ON storage_limits (tenant_id, COALESCE(project_id, 0));

GRANT SELECT, INSERT, UPDATE, DELETE ON storage_limits TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE storage_limits_id_seq TO ggscale_app;
