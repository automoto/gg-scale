-- Restore the original per-project end_user-keyed friend_edges (schema from
-- migrations 0006 + 0009 + 0046). No data is preserved.
DROP TABLE IF EXISTS friend_edges;

CREATE TABLE friend_edges (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    from_user_id    BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    to_user_id      BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    status          TEXT NOT NULL CHECK (status IN ('pending', 'accepted', 'rejected', 'blocked')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT friend_edges_no_self_loop CHECK (from_user_id <> to_user_id)
);

CREATE UNIQUE INDEX friend_edges_pair_uniq
    ON friend_edges (tenant_id, from_user_id, to_user_id);
CREATE INDEX friend_edges_tenant_id_idx ON friend_edges (tenant_id);
CREATE INDEX friend_edges_to_user_idx
    ON friend_edges (tenant_id, to_user_id, status);

ALTER TABLE friend_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE friend_edges FORCE ROW LEVEL SECURITY;
CREATE POLICY friend_edges_isolation ON friend_edges
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON friend_edges TO ggscale_app;
GRANT USAGE, SELECT ON friend_edges_id_seq TO ggscale_app;
