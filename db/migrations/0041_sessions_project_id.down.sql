DROP INDEX IF EXISTS sessions_project_active_idx;

ALTER TABLE sessions
    DROP COLUMN IF EXISTS project_id;
