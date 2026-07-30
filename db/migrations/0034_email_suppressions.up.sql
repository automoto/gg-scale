-- Platform-wide invite-email suppression list, fed by the one-click
-- unsubscribe link in every invite email. Keyed by normalized address
-- (citext = case-insensitive); invite senders check it before sending and
-- drop the email while still creating the invite itself.
CREATE TABLE email_suppressions (
    email      CITEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, DELETE ON email_suppressions TO ggscale_app;
