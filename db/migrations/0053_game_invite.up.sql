-- Game session invites: one player invites another to join a specific
-- xsession. These are short-lived (5 min TTL) and distinct from
-- end_user_invitations (which are player-registration invites).

CREATE TABLE game_invite (
    id           BIGSERIAL PRIMARY KEY,
    tenant_id    BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id   BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    from_user_id BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    to_user_id   BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    session_id   TEXT NOT NULL,
    join_code    TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);

-- Lookup by recipient + expiry. now() is not IMMUTABLE so cannot be used
-- in a partial index predicate; the full index is fine — the query WHERE
-- clause filters expired rows and the planner uses the expires_at column.
CREATE INDEX game_invite_to_user_idx
    ON game_invite (tenant_id, to_user_id, expires_at);

CREATE INDEX game_invite_tenant_idx ON game_invite (tenant_id);

ALTER TABLE game_invite ENABLE ROW LEVEL SECURITY;
ALTER TABLE game_invite FORCE ROW LEVEL SECURITY;
CREATE POLICY game_invite_isolation ON game_invite
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON game_invite TO ggscale_app;
GRANT USAGE, SELECT ON game_invite_id_seq TO ggscale_app;
