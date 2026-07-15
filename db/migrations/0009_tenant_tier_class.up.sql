-- Tier rename: string enum (free/payg/premium) → numbered integer classes 0..3.
-- Deliberate pre-1.0 break (docs/temp/tier-rework.md M1). Numbered classes carry
-- no price judgment; human labels and dollars live in the commercial layer.
-- Backfill free→0, payg→1, premium→2; the new default class is 0.
ALTER TABLE tenants
    ADD COLUMN tier_class smallint NOT NULL DEFAULT 0
        CHECK (tier_class BETWEEN 0 AND 3);

UPDATE tenants SET tier_class = CASE tier
    WHEN 'free'    THEN 0
    WHEN 'payg'    THEN 1
    WHEN 'premium' THEN 2
    ELSE 0
END;

ALTER TABLE tenants DROP CONSTRAINT tenants_tier_check;
ALTER TABLE tenants DROP COLUMN tier;
ALTER TABLE tenants RENAME COLUMN tier_class TO tier;
