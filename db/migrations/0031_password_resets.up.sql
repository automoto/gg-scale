-- Forgot-password reset tokens for both auth stacks. The emailed token is
-- random and stored only as a sha-256 hash; rows are single-use (used_at)
-- and expire. Two tables instead of one polymorphic table: the FK types
-- differ (bigint vs uuid) and each stack consumes only its own rows.
CREATE TABLE control_panel_password_resets (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    control_panel_user_id BIGINT NOT NULL REFERENCES control_panel_users(id) ON DELETE CASCADE,
    token_hash            BYTEA NOT NULL UNIQUE,
    expires_at            TIMESTAMPTZ NOT NULL,
    used_at               TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE player_account_password_resets (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    player_account_id UUID NOT NULL REFERENCES player_accounts(id) ON DELETE CASCADE,
    token_hash        BYTEA NOT NULL UNIQUE,
    expires_at        TIMESTAMPTZ NOT NULL,
    used_at           TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE, DELETE ON control_panel_password_resets TO ggscale_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON player_account_password_resets TO ggscale_app;
