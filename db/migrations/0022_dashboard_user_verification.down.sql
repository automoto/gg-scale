ALTER TABLE dashboard_users
    DROP COLUMN email_verification_last_sent_at,
    DROP COLUMN email_verification_attempts,
    DROP COLUMN email_verification_expires_at,
    DROP COLUMN email_verification_salt,
    DROP COLUMN email_verification_code_hash,
    DROP COLUMN email_verified_at;
