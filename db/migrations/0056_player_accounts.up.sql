-- Global player accounts.
--
-- player_accounts is a PLATFORM-GLOBAL identity that sits above the
-- per-project end_users rows. It has no tenant_id and no row-level-security
-- policy — exactly like dashboard_users — and is only ever touched through
-- db.Pool.BootstrapQ (the no-app.tenant_id path). See
-- docs/temp/player-accounts.md for the full rationale (why it is safe outside
-- tenant RLS, the session model, and the explicit-linking rule).
--
-- No live data exists yet, so this migration is destructive-friendly but must
-- run cleanly from zero.

-- id is a UUID (global, non-enumerable). The verification columns mirror
-- dashboard_users (migrations 0022 + 0035) so the internal/verifycode
-- lock/resend logic is shared.
CREATE TABLE player_accounts (
    id                                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email                                 CITEXT NOT NULL UNIQUE,
    password_hash                         BYTEA NOT NULL,
    display_name                          TEXT,
    email_verified_at                     TIMESTAMPTZ,
    disabled_at                           TIMESTAMPTZ,
    -- Bumped on password change / disable; the account-session cookie stores
    -- the epoch it was minted at and is rejected when this moves.
    session_epoch                         INTEGER NOT NULL DEFAULT 0,
    email_verification_code_hash          BYTEA,
    email_verification_salt               BYTEA,
    email_verification_expires_at         TIMESTAMPTZ,
    email_verification_attempts           INTEGER NOT NULL DEFAULT 0,
    email_verification_lifetime_attempts  INTEGER NOT NULL DEFAULT 0,
    email_verification_locked_until       TIMESTAMPTZ,
    email_verification_last_sent_at       TIMESTAMPTZ,
    created_at                            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Account sessions are global (no tenant, no end_user). Only the refresh-token
-- hash is stored; session_epoch is snapshotted so a bumped account epoch
-- invalidates every outstanding session on the next request.
CREATE TABLE player_account_sessions (
    id                 BIGSERIAL PRIMARY KEY,
    player_account_id  UUID NOT NULL REFERENCES player_accounts(id) ON DELETE CASCADE,
    refresh_hash       BYTEA NOT NULL UNIQUE,
    session_epoch      INTEGER NOT NULL DEFAULT 0,
    expires_at         TIMESTAMPTZ NOT NULL,
    revoked_at         TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX player_account_sessions_account_active_idx
    ON player_account_sessions (player_account_id)
    WHERE revoked_at IS NULL;

-- The per-project game identity links up to a global account. Nullable: an
-- anonymous / unlinked end_user keeps working with no account. ON DELETE SET
-- NULL keeps gameplay data alive if an account is ever hard-deleted.
ALTER TABLE end_users
    ADD COLUMN player_account_id UUID REFERENCES player_accounts(id) ON DELETE SET NULL;

CREATE INDEX end_users_player_account_id_idx
    ON end_users (player_account_id)
    WHERE player_account_id IS NOT NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON player_accounts TO ggscale_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON player_account_sessions TO ggscale_app;
GRANT USAGE, SELECT ON player_account_sessions_id_seq TO ggscale_app;

-- Listing the projects an account is linked to is inherently cross-tenant, so
-- it cannot run under end_users' tenant RLS. This SECURITY DEFINER helper is
-- the single controlled surface for that read: it is keyed by account id
-- (never a tenant scan) and only returns rows the account itself is linked to,
-- so it widens visibility no further than the account already owns. Mirrors
-- the player_end_user_tenant helper from migration 0027.
CREATE OR REPLACE FUNCTION player_account_linked_projects(p_account_id UUID)
RETURNS TABLE (
    end_user_id BIGINT,
    tenant_id   BIGINT,
    project_id  BIGINT,
    project_name TEXT,
    external_id TEXT,
    linked_at   TIMESTAMPTZ
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT e.id, e.tenant_id, e.project_id, p.name::text, e.external_id, e.created_at
    FROM end_users e
    JOIN projects p ON p.id = e.project_id
    WHERE e.player_account_id = p_account_id
      AND e.deleted_at IS NULL
    ORDER BY e.created_at;
$$;

REVOKE ALL ON FUNCTION player_account_linked_projects(UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION player_account_linked_projects(UUID) TO ggscale_app;
