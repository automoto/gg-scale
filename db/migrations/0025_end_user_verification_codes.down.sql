DROP INDEX IF EXISTS end_users_email_verification_code_idx;

ALTER TABLE end_users
    DROP COLUMN email_verification_last_sent_at,
    DROP COLUMN email_verification_attempts,
    DROP COLUMN email_verification_salt;

ALTER TABLE end_users
    RENAME COLUMN email_verification_code_hash TO email_verification_hash;

CREATE INDEX end_users_email_verification_idx
    ON end_users (email_verification_hash)
    WHERE email_verification_hash IS NOT NULL;
