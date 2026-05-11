-- Fleet allocations are short-lived game-server slots requested by the
-- matchmaker. Each row tracks one server through its lifecycle
-- (pending → allocating → ready → allocated → draining → shutdown / failed).
-- `backend_ref` is the backend-specific identifier (Docker container ID,
-- Agones GameServer name, OpenStack instance UUID, plugin-supplied opaque
-- string) and is opaque to ggscale.

CREATE TYPE allocation_status AS ENUM (
    'pending',
    'allocating',
    'ready',
    'allocated',
    'draining',
    'shutdown',
    'failed'
);

CREATE TABLE game_server_allocations (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id    BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    backend       TEXT NOT NULL,
    backend_ref   TEXT NOT NULL DEFAULT '',
    region        TEXT NOT NULL DEFAULT '',
    address       TEXT NOT NULL DEFAULT '',
    status        allocation_status NOT NULL DEFAULT 'pending',
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
    requested_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at      TIMESTAMPTZ,
    released_at   TIMESTAMPTZ
);

CREATE INDEX game_server_allocations_tenant_id_idx
    ON game_server_allocations (tenant_id);

CREATE INDEX game_server_allocations_active_idx
    ON game_server_allocations (tenant_id, project_id, status)
    WHERE released_at IS NULL;

ALTER TABLE game_server_allocations ENABLE ROW LEVEL SECURITY;
CREATE POLICY game_server_allocations_isolation ON game_server_allocations
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);
