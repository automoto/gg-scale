-- Per-session peer roster. Each row holds a participant's public UDP
-- endpoint (ip:port) and last-seen heartbeat. Rows are TTL-evicted via
-- last_seen: the heartbeat handler prunes peers that haven't checked in
-- within 30 s.
--
-- Platform-specific secure-association data (e.g. an Xbox XNADDR/XNKID/
-- XNKEY triple) is intentionally NOT stored here. ggscale brokers only the
-- generic endpoint; a deployment-specific shim/proxy handles any opaque
-- key material and forwards the public address.

CREATE TABLE game_session_peer (
    tenant_id   BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    session_id  TEXT   NOT NULL REFERENCES game_session(id) ON DELETE CASCADE,
    end_user_id BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    ip          TEXT,
    port        INT,
    qos         JSONB  NOT NULL DEFAULT '{}',
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, end_user_id)
);

CREATE INDEX game_session_peer_session_idx ON game_session_peer (session_id, last_seen);

-- Peer rows carry tenant_id and are RLS-isolated like every other
-- tenant-scoped table. Peer queries are keyed by session_id (a 128-bit
-- random token), but the RLS policy is the real isolation boundary so a
-- known session ID can never expose another tenant's peer key material.
ALTER TABLE game_session_peer ENABLE ROW LEVEL SECURITY;
ALTER TABLE game_session_peer FORCE ROW LEVEL SECURITY;
CREATE POLICY game_session_peer_isolation ON game_session_peer
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON game_session_peer TO ggscale_app;
