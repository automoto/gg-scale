CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX dashboard_users_email_trgm_idx
    ON dashboard_users USING gin ((email::text) gin_trgm_ops);

CREATE INDEX dashboard_users_created_id_idx
    ON dashboard_users (created_at DESC, id DESC);

CREATE INDEX end_users_project_email_trgm_idx
    ON end_users USING gin ((email::text) gin_trgm_ops)
    WHERE deleted_at IS NULL AND email IS NOT NULL;

CREATE INDEX end_users_project_created_id_idx
    ON end_users (project_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

CREATE INDEX game_server_allocations_project_id_desc_idx
    ON game_server_allocations (tenant_id, project_id, id DESC);

CREATE INDEX game_server_allocations_project_live_id_desc_idx
    ON game_server_allocations (tenant_id, project_id, id DESC)
    WHERE released_at IS NULL;
