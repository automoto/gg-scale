-- Server-generated secrets that must survive restarts and be shared by
-- every instance on this database. First use: the auto-generated two-factor
-- encryption key (name 'two_factor_enc_key'), created on first boot when
-- TWO_FACTOR_ENC_KEY is not set so 2FA works with zero configuration.
--
-- Platform-global: no tenant_id, no RLS, only touched through BootstrapQ.
--
-- The app role deliberately gets no UPDATE/DELETE: a stored key is
-- immutable at the DB layer, so no code path can silently replace it and
-- strand the ciphertexts sealed under it. Removal is an operator action
-- with the owner role.

CREATE TABLE server_secrets (
    name       TEXT PRIMARY KEY,
    value      BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT ON server_secrets TO ggscale_app;
