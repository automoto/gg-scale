ALTER TABLE tenants DROP COLUMN IF EXISTS custom_token_secret;

DROP INDEX IF EXISTS end_users_email_verification_idx;
ALTER TABLE end_users
    DROP COLUMN IF EXISTS email_verification_expires_at,
    DROP COLUMN IF EXISTS email_verification_hash;
