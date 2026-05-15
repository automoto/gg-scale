-- Project admins can disable a player. Disabled players cannot log in but
-- their data (friends, scores, etc.) is retained; un-disabling restores
-- access without re-running the signup flow.

ALTER TABLE end_users
    ADD COLUMN disabled_at TIMESTAMPTZ;

CREATE INDEX end_users_disabled_idx
    ON end_users (disabled_at)
    WHERE disabled_at IS NOT NULL;
