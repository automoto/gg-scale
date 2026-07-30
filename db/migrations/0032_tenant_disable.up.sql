-- Tenant disable state. disabled_by records who disabled the tenant because
-- the re-enable rules differ: a tenant self-disable can be undone by a tenant
-- admin, a platform disable only by a platform admin (and it also locks
-- tenant admins out of the tenant's control-panel pages). While disabled,
-- the tenant's API keys stop resolving, which blocks all player traffic.
ALTER TABLE tenants
    ADD COLUMN disabled_at timestamptz,
    ADD COLUMN disabled_by TEXT CHECK (disabled_by IN ('tenant', 'platform')),
    ADD CONSTRAINT tenants_disabled_pair_check CHECK ((disabled_at IS NULL) = (disabled_by IS NULL));
