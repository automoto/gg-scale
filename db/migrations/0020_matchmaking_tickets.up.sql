-- Matchmaking tickets queue. An end-user posts a ticket via /v1/matchmaker;
-- the worker batches tickets into buckets keyed by (region, game_mode) and
-- calls fleet.Manager.Allocate once enough have accumulated. The ticket then
-- carries the allocated server's address back to the player over the WS hub.

CREATE TYPE ticket_status AS ENUM (
    'queued',
    'matched',
    'cancelled',
    'failed'
);

CREATE TABLE matchmaking_tickets (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id    BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    end_user_id   BIGINT NOT NULL,
    region        TEXT NOT NULL DEFAULT '',
    game_mode     TEXT NOT NULL DEFAULT '',
    attributes    JSONB NOT NULL DEFAULT '{}'::jsonb,
    status        ticket_status NOT NULL DEFAULT 'queued',
    match_address TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    matched_at    TIMESTAMPTZ
);

CREATE INDEX matchmaking_tickets_tenant_id_idx
    ON matchmaking_tickets (tenant_id);

-- The worker scans queued tickets by (region, game_mode) buckets; this
-- partial index keeps the scan O(queue depth) instead of O(history).
CREATE INDEX matchmaking_tickets_queued_idx
    ON matchmaking_tickets (region, game_mode, created_at)
    WHERE status = 'queued';

ALTER TABLE matchmaking_tickets ENABLE ROW LEVEL SECURITY;
CREATE POLICY matchmaking_tickets_isolation ON matchmaking_tickets
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON matchmaking_tickets TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE matchmaking_tickets_id_seq TO ggscale_app;
