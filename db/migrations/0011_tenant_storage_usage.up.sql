-- Per-tenant object-storage metering (docs/temp/tier-rework.md M4). A byte
-- counter maintained transactionally by the storage write paths in the same tx
-- as the object write, so it cannot drift. Platform-global (no RLS): the counter
-- is upserted in tenant context and the storage-warn River job scans it across
-- tenants; every app read filters by app.tenant_id explicitly.
CREATE TABLE tenant_storage_usage (
    tenant_id               BIGINT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    total_bytes             BIGINT NOT NULL DEFAULT 0,
    -- Highest threshold (0 = none, 80, 100) last emailed to tenant admins, so
    -- the warn job doesn't re-mail every run. Reset when usage falls below 80%.
    last_notified_threshold SMALLINT NOT NULL DEFAULT 0,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_storage_usage TO ggscale_app;

-- Backfill the baseline from existing live objects. Size is measured as
-- octet_length(value::text) — the canonical JSON text length — consistently
-- here and in the write-path delta accounting.
INSERT INTO tenant_storage_usage (tenant_id, total_bytes)
SELECT tenant_id, COALESCE(SUM(octet_length(value::text)), 0)
FROM storage_objects
WHERE deleted_at IS NULL
GROUP BY tenant_id;
