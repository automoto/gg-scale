-- Platform admins can disable a dashboard user. Disabled accounts cannot
-- log in (login treats them as unknown email) and cannot hold an active
-- dashboard session (the session-lookup query filters them out and active
-- sessions are revoked on the disable transaction). Reversible: re-enable
-- clears disabled_at; previously-revoked sessions and revoked outgoing
-- invitations are NOT restored.

ALTER TABLE dashboard_users
    ADD COLUMN disabled_at TIMESTAMPTZ;

CREATE INDEX dashboard_users_disabled_idx
    ON dashboard_users (disabled_at)
    WHERE disabled_at IS NOT NULL;
