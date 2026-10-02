-- One-time tickets for the realtime WebSocket. A browser cannot set request
-- headers on a WebSocket, so a game gets a ticket over authenticated HTTP and
-- opens /v1/ws?ticket=... with it. Only the SHA-256 of the ticket is stored.
-- api_key_hash is the same value as api_keys.key_hash: redemption resolves
-- the key again, so a key revoked after the ticket was made still fails.
CREATE TABLE realtime_tickets (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id     BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    ticket_hash   BYTEA NOT NULL,
    api_key_hash  BYTEA NOT NULL,
    player_id     BIGINT NOT NULL,
    project_id    BIGINT NOT NULL DEFAULT 0,
    session_epoch BIGINT NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX realtime_tickets_hash_uniq ON realtime_tickets (ticket_hash);
CREATE INDEX realtime_tickets_expires_idx ON realtime_tickets (expires_at);

ALTER TABLE realtime_tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE realtime_tickets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON realtime_tickets
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);
-- Redemption and the cleanup job run before a tenant is known.
CREATE POLICY worker_access ON realtime_tickets
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NULL);

GRANT SELECT, INSERT, DELETE ON realtime_tickets TO ggscale_app;
