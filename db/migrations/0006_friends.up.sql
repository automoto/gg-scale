-- Directed friend edges. The application enforces symmetric pairing for
-- accepted edges; the constraint here just blocks self-edges and lets the
-- (from,to) uniqueness handle dedupe.

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
