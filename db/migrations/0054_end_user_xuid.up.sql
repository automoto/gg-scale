-- Optional secondary identifier for a player. Self-set by the player via
-- PATCH /v1/profile and surfaced in friends/peer rosters. Distinct from
-- external_id (the tenant's primary auth identifier): xuid is a free-form
-- display/lookup handle with no authentication authority.

ALTER TABLE end_users ADD COLUMN xuid TEXT;

-- One xuid per project among live users; NULL is unconstrained so most
-- players (who never set one) don't collide.
CREATE UNIQUE INDEX end_users_xuid_uniq
    ON end_users (project_id, xuid)
    WHERE xuid IS NOT NULL AND deleted_at IS NULL;
