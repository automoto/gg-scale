-- Game session registry. The game's network layer announces a session
-- here (host) and peers join by session ID or join_code. The backend
-- stores the network address blobs and key material so peers can find
-- each other. Distinct from the auth `sessions` table (refresh tokens).

CREATE TABLE game_session (
    id           TEXT PRIMARY KEY,
    join_code    TEXT NOT NULL UNIQUE,
    tenant_id    BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id   BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title_id     TEXT NOT NULL DEFAULT '',
    host_user_id BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    state        TEXT NOT NULL DEFAULT 'open'
                 CHECK (state IN ('open', 'in_progress', 'ended')),
    props        JSONB NOT NULL DEFAULT '{}',
    max_players  INT NOT NULL DEFAULT 2,
    private      BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX game_session_tenant_state_idx ON game_session (tenant_id, state);
-- Per-project open-session cap counts by (project_id, state, expires_at).
CREATE INDEX game_session_project_state_idx ON game_session (project_id, state, expires_at);
-- Partial index: join-code lookups only care about open sessions.
CREATE INDEX game_session_join_code_idx ON game_session (join_code) WHERE state = 'open';

ALTER TABLE game_session ENABLE ROW LEVEL SECURITY;
ALTER TABLE game_session FORCE ROW LEVEL SECURITY;
CREATE POLICY game_session_isolation ON game_session
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON game_session TO ggscale_app;
