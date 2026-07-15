-- Restore the string enum. Lossy for tier_3 (no legacy string existed): it
-- collapses to 'premium', the closest paid class.
ALTER TABLE tenants
    ADD COLUMN tier_str text NOT NULL DEFAULT 'free'
        CHECK (tier_str IN ('free', 'payg', 'premium'));

UPDATE tenants SET tier_str = CASE tier
    WHEN 0 THEN 'free'
    WHEN 1 THEN 'payg'
    WHEN 2 THEN 'premium'
    ELSE 'premium'
END;

ALTER TABLE tenants DROP COLUMN tier;
ALTER TABLE tenants RENAME COLUMN tier_str TO tier;
ALTER TABLE tenants RENAME CONSTRAINT tenants_tier_str_check TO tenants_tier_check;
