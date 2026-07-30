ALTER TABLE tenants
    DROP CONSTRAINT tenants_disabled_pair_check,
    DROP COLUMN disabled_by,
    DROP COLUMN disabled_at;
