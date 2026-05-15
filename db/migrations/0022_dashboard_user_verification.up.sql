-- Dashboard users get a 6-digit code email verification flow. New columns:
--   * email_verified_at: nullable; populated when verify succeeds.
--   * email_verification_code_hash: SHA-256 of "<salt>:<code>", 32 bytes.
--   * email_verification_salt: 16-byte per-user salt so brute force can't
--     precompute a single rainbow table for the 10^6 code space.
--   * email_verification_expires_at: 15-minute window typical.
--   * email_verification_attempts: increments on each verify attempt; the
--     row locks out (cleared, must resend) after 5.
--   * email_verification_last_sent_at: powers the 1/min resend rate limit.
--
-- Existing rows are bootstrap admins from setup/setup-token; mark them
-- already verified so they keep working.

ALTER TABLE dashboard_users
    ADD COLUMN email_verified_at               TIMESTAMPTZ,
    ADD COLUMN email_verification_code_hash    BYTEA,
    ADD COLUMN email_verification_salt         BYTEA,
    ADD COLUMN email_verification_expires_at   TIMESTAMPTZ,
    ADD COLUMN email_verification_attempts     INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN email_verification_last_sent_at TIMESTAMPTZ;

UPDATE dashboard_users
SET email_verified_at = created_at
WHERE email_verified_at IS NULL;
