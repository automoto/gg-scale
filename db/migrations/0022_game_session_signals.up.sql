CREATE TABLE game_session_signal (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    session_id     TEXT NOT NULL REFERENCES game_session(id) ON DELETE CASCADE,
    from_player_id BIGINT NOT NULL,
    to_player_id   BIGINT NOT NULL,
    negotiation_id TEXT NOT NULL CHECK (char_length(negotiation_id) BETWEEN 1 AND 128),
    kind           TEXT NOT NULL CHECK (kind IN ('offer', 'answer', 'restart_offer', 'restart_answer')),
    payload        TEXT NOT NULL CHECK (octet_length(payload) BETWEEN 1 AND 65536),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL DEFAULT now() + interval '2 minutes'
);

CREATE INDEX game_session_signal_recipient_idx
    ON game_session_signal (tenant_id, session_id, to_player_id, id);

ALTER TABLE game_session_signal ENABLE ROW LEVEL SECURITY;
ALTER TABLE game_session_signal FORCE ROW LEVEL SECURITY;
CREATE POLICY game_session_signal_isolation
    ON game_session_signal
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT ON game_session_signal TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE game_session_signal_id_seq TO ggscale_app;
