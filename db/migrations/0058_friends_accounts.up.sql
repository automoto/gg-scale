-- Rework friend_edges to reference GLOBAL player_accounts instead of
-- per-project end_users (Milestone 4). Friends are platform-wide: the same
-- human is one account across every game, so friendships live between
-- account UUIDs, not tenant-scoped end_user rows.
--
-- No live data — drop and recreate. friend_edges becomes platform-global
-- (no tenant_id, no RLS), exactly like player_accounts. Because it carries no
-- RLS policy it is readable/writable by ggscale_app inside BOTH a tenant-
-- scoped Pool.Q transaction and a BootstrapQ transaction, so it can be joined
-- against RLS-filtered end_users for the per-project block/presence checks.

DROP TABLE IF EXISTS friend_edges;

CREATE TABLE friend_edges (
    id               BIGSERIAL PRIMARY KEY,
    from_account_id  UUID NOT NULL REFERENCES player_accounts(id) ON DELETE CASCADE,
    to_account_id    UUID NOT NULL REFERENCES player_accounts(id) ON DELETE CASCADE,
    status           TEXT NOT NULL CHECK (status IN ('pending', 'accepted', 'rejected', 'blocked')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT friend_edges_no_self_loop CHECK (from_account_id <> to_account_id)
);

CREATE UNIQUE INDEX friend_edges_pair_uniq
    ON friend_edges (from_account_id, to_account_id);
CREATE INDEX friend_edges_to_account_idx
    ON friend_edges (to_account_id, status);

GRANT SELECT, INSERT, UPDATE, DELETE ON friend_edges TO ggscale_app;
GRANT USAGE, SELECT ON friend_edges_id_seq TO ggscale_app;
