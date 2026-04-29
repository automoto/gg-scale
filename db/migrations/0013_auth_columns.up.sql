-- Auth surface needs three more pieces of state beyond what 0003/0002 set up:
--   1. an opaque verification-token hash (and expiry) on end_users so
--      POST /v1/auth/verify can match a clicked link to the row to flip
--      email_verified_at;
--   2. a per-tenant HMAC secret on tenants for POST /v1/auth/custom-token
--      (the integrator signs a JWT for their player; ggscale verifies and
--      mints a session). HS256 keeps Phase 1 simple — RS256 follows when
--      we add a key-rotation API.

ALTER TABLE end_users
    ADD COLUMN email_verification_hash       BYTEA,
    ADD COLUMN email_verification_expires_at TIMESTAMPTZ;

CREATE INDEX end_users_email_verification_idx
    ON end_users (email_verification_hash)
    WHERE email_verification_hash IS NOT NULL;

ALTER TABLE tenants
    ADD COLUMN custom_token_secret BYTEA;
