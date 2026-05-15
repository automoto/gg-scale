-- Aggressive refactor (no live users): swap end_user email verification
-- from 32-byte opaque hex token to 6-digit numeric code, matching the
-- dashboard flow. Old columns are dropped/renamed in-place because there
-- are no existing rows in production to migrate.
--
--   * email_verification_hash       -> email_verification_code_hash
--   * (new) email_verification_salt
--   * (new) email_verification_attempts
--   * (new) email_verification_last_sent_at

DROP INDEX IF EXISTS end_users_email_verification_idx;

ALTER TABLE end_users
    RENAME COLUMN email_verification_hash TO email_verification_code_hash;

ALTER TABLE end_users
    ADD COLUMN email_verification_salt          BYTEA,
    ADD COLUMN email_verification_attempts      INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN email_verification_last_sent_at  TIMESTAMPTZ;

CREATE INDEX end_users_email_verification_code_idx
    ON end_users (email_verification_code_hash)
    WHERE email_verification_code_hash IS NOT NULL;
