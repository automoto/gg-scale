ALTER TABLE project_players
DROP COLUMN password_reset_last_sent_at,
DROP COLUMN password_reset_locked_until,
DROP COLUMN password_reset_lifetime_attempts,
DROP COLUMN password_reset_attempts,
DROP COLUMN password_reset_expires_at,
DROP COLUMN password_reset_salt,
DROP COLUMN password_reset_code_hash;
