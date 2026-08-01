-- Per-project Steam sign-in credentials, edited in the control panel under
-- the existing project:*:config permission. steam_web_api_key is the
-- publisher Web API key: write-only from the UI, never selected by
-- control-panel queries. The bytea holds the key sealed by the credential
-- cipher (internal/secretseal, CREDENTIAL_ENC_KEY), not plaintext.
ALTER TABLE projects
ADD COLUMN steam_app_id text NOT NULL DEFAULT '',
ADD COLUMN steam_web_api_key bytea;
