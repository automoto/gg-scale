-- Per-code attempts (email_verification_attempts) bounds tries against a
-- single code, but it resets to 0 every time a new code is minted by
-- /resend. That defeats the bound: an attacker just calls /resend, gets a
-- fresh code, and starts over. We add a lifetime counter that survives
-- resends and a lockout timestamp consulted by the resend handler.
--
-- The lockout window is operator-tunable but defaults to 24h in code.
-- Reaching the lifetime cap (default 20) flips an account into "locked"
-- and forces the operator to clear it manually — mirroring the existing
-- login_failures lockout.

ALTER TABLE dashboard_users
    ADD COLUMN email_verification_lifetime_attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN email_verification_locked_until      TIMESTAMPTZ;

ALTER TABLE end_users
    ADD COLUMN email_verification_lifetime_attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN email_verification_locked_until      TIMESTAMPTZ;
