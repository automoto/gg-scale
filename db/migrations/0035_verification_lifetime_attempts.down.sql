ALTER TABLE end_users
    DROP COLUMN email_verification_locked_until,
    DROP COLUMN email_verification_lifetime_attempts;

ALTER TABLE dashboard_users
    DROP COLUMN email_verification_locked_until,
    DROP COLUMN email_verification_lifetime_attempts;
