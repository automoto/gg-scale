-- Leaderboards belong to a project. Entries are append-only; "best score
-- per user" is computed at read time so we keep an audit trail of submissions.

CREATE TABLE leaderboards (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id  BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    sort_order  TEXT NOT NULL DEFAULT 'desc' CHECK (sort_order IN ('asc', 'desc')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

CREATE INDEX leaderboards_tenant_id_idx ON leaderboards (tenant_id);
CREATE UNIQUE INDEX leaderboards_name_uniq
    ON leaderboards (tenant_id, project_id, name)
    WHERE deleted_at IS NULL;

CREATE TABLE leaderboard_entries (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    leaderboard_id  BIGINT NOT NULL REFERENCES leaderboards(id) ON DELETE CASCADE,
    end_user_id     BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    score           BIGINT NOT NULL,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX leaderboard_entries_tenant_id_idx ON leaderboard_entries (tenant_id);
CREATE INDEX leaderboard_entries_top_idx
    ON leaderboard_entries (tenant_id, leaderboard_id, score DESC, recorded_at);
CREATE INDEX leaderboard_entries_user_idx
    ON leaderboard_entries (tenant_id, leaderboard_id, end_user_id);
