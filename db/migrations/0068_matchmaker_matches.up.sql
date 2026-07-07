-- Committed match results. One row per match; ticket rows point at it via
-- match_id. Lets players recover a matched result by polling their ticket
-- after a missed WebSocket delivery, and gives operators a match log.
-- Rows are retention-bounded (expires_at) and GC'd by a River job.

CREATE TABLE matchmaker_matches (
    id         TEXT PRIMARY KEY,
    tenant_id  BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    mode       TEXT NOT NULL,
    fleet_id   BIGINT,
    address    TEXT NOT NULL DEFAULT '',
    protocol   TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    join_code  TEXT NOT NULL DEFAULT '',
    roster     JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX matchmaker_matches_expires_idx ON matchmaker_matches (expires_at);

ALTER TABLE matchmaker_matches ENABLE ROW LEVEL SECURITY;
ALTER TABLE matchmaker_matches FORCE ROW LEVEL SECURITY;
CREATE POLICY matchmaker_matches_isolation ON matchmaker_matches
    FOR ALL
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON matchmaker_matches TO ggscale_app;
