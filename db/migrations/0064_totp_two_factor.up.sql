-- Optional TOTP two-factor auth for dashboard users and player accounts.
--
-- Three tables per surface (credential, backup codes, trusted devices),
-- platform-global like their parent identity tables: no tenant_id, no RLS,
-- only touched through BootstrapQ. Dedicated tables rather than columns on
-- the (already wide) user tables: "enabled" is row-exists + confirmed_at,
-- and disable is a single CASCADE delete.
--
-- secret_enc is opaque to SQL: version byte || AES-GCM nonce || ciphertext,
-- framed and keyed by internal/twofactor (TWO_FACTOR_ENC_KEY).

CREATE TABLE dashboard_user_totp (
    dashboard_user_id  BIGINT PRIMARY KEY REFERENCES dashboard_users(id) ON DELETE CASCADE,
    secret_enc         BYTEA NOT NULL,
    -- NULL until the user proves possession with a first valid code.
    confirmed_at       TIMESTAMPTZ,
    -- Timestep of the last accepted code; codes at or before it are replays.
    last_used_step     BIGINT NOT NULL DEFAULT 0,
    attempts           INTEGER NOT NULL DEFAULT 0,
    locked_until       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE dashboard_user_totp_backup_codes (
    id                 BIGSERIAL PRIMARY KEY,
    dashboard_user_id  BIGINT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
    code_hash          BYTEA NOT NULL,
    used_at            TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dashboard_user_id, code_hash)
);

CREATE TABLE dashboard_trusted_devices (
    id                 BIGSERIAL PRIMARY KEY,
    dashboard_user_id  BIGINT NOT NULL REFERENCES dashboard_users(id) ON DELETE CASCADE,
    token_hash         BYTEA NOT NULL UNIQUE,
    expires_at         TIMESTAMPTZ NOT NULL,
    ip                 TEXT,
    user_agent         TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX dashboard_trusted_devices_user_idx
    ON dashboard_trusted_devices (dashboard_user_id);

CREATE TABLE player_account_totp (
    player_account_id  UUID PRIMARY KEY REFERENCES player_accounts(id) ON DELETE CASCADE,
    secret_enc         BYTEA NOT NULL,
    confirmed_at       TIMESTAMPTZ,
    last_used_step     BIGINT NOT NULL DEFAULT 0,
    attempts           INTEGER NOT NULL DEFAULT 0,
    locked_until       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE player_account_totp_backup_codes (
    id                 BIGSERIAL PRIMARY KEY,
    player_account_id  UUID NOT NULL REFERENCES player_accounts(id) ON DELETE CASCADE,
    code_hash          BYTEA NOT NULL,
    used_at            TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (player_account_id, code_hash)
);

-- No ip/user_agent: player_account_sessions doesn't record them either.
CREATE TABLE player_account_trusted_devices (
    id                 BIGSERIAL PRIMARY KEY,
    player_account_id  UUID NOT NULL REFERENCES player_accounts(id) ON DELETE CASCADE,
    token_hash         BYTEA NOT NULL UNIQUE,
    expires_at         TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX player_account_trusted_devices_account_idx
    ON player_account_trusted_devices (player_account_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON dashboard_user_totp TO ggscale_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON dashboard_user_totp_backup_codes TO ggscale_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON dashboard_trusted_devices TO ggscale_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON player_account_totp TO ggscale_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON player_account_totp_backup_codes TO ggscale_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON player_account_trusted_devices TO ggscale_app;
GRANT USAGE, SELECT ON dashboard_user_totp_backup_codes_id_seq TO ggscale_app;
GRANT USAGE, SELECT ON dashboard_trusted_devices_id_seq TO ggscale_app;
GRANT USAGE, SELECT ON player_account_totp_backup_codes_id_seq TO ggscale_app;
GRANT USAGE, SELECT ON player_account_trusted_devices_id_seq TO ggscale_app;
