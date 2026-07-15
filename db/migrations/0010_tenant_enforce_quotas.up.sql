-- Per-tenant opt-in for quota enforcement. Zero-config self-host stays
-- uncapped (docs/temp/tier-rework.md M2): quotas apply only where this is true.
-- The DoS token bucket and connection cap are always on, independent of this.
ALTER TABLE tenants ADD COLUMN enforce_quotas boolean NOT NULL DEFAULT false;
