-- Single sign-on connections. An account can sign in with a provider
-- identity in place of (or next to) a password, so password_hash becomes
-- optional on both identity stores. Two tables instead of one polymorphic
-- table: the FK types differ (uuid vs bigint) and the stores stay separate.
ALTER TABLE player_accounts ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE control_panel_users ALTER COLUMN password_hash DROP NOT NULL;

CREATE TABLE player_account_connections (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    player_account_id UUID NOT NULL REFERENCES player_accounts(id) ON DELETE CASCADE,
    provider          TEXT NOT NULL CHECK (provider IN ('google', 'steam', 'discord')),
    subject           TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at      TIMESTAMPTZ,
    UNIQUE (provider, subject),
    UNIQUE (player_account_id, provider)
);

CREATE TABLE control_panel_user_connections (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    control_panel_user_id BIGINT NOT NULL REFERENCES control_panel_users(id) ON DELETE CASCADE,
    provider              TEXT NOT NULL CHECK (provider IN ('google', 'github')),
    subject               TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at          TIMESTAMPTZ,
    UNIQUE (provider, subject),
    UNIQUE (control_panel_user_id, provider)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON player_account_connections TO ggscale_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON control_panel_user_connections TO ggscale_app;
