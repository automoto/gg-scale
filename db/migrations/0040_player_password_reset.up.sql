-- In-client password reset for project_players, mirroring the
-- email_verification_* challenge columns: a 6-digit emailed code with salted
-- hash, TTL, per-code and lifetime attempt caps, lockout, and a resend
-- cooldown. Games cannot open browser links, so the hosted tiers'
-- token-link reset tables do not fit here.
ALTER TABLE project_players
ADD COLUMN password_reset_code_hash bytea,
ADD COLUMN password_reset_salt bytea,
ADD COLUMN password_reset_expires_at timestamptz,
ADD COLUMN password_reset_attempts integer NOT NULL DEFAULT 0,
ADD COLUMN password_reset_lifetime_attempts integer NOT NULL DEFAULT 0,
ADD COLUMN password_reset_locked_until timestamptz,
ADD COLUMN password_reset_last_sent_at timestamptz;
