ALTER TABLE sessions
    ADD COLUMN project_id BIGINT REFERENCES projects(id) ON DELETE CASCADE;

UPDATE sessions s
SET project_id = u.project_id
FROM end_users u
WHERE u.id = s.end_user_id
  AND s.project_id IS NULL;

ALTER TABLE sessions
    ALTER COLUMN project_id SET NOT NULL;

CREATE INDEX sessions_project_active_idx
    ON sessions (tenant_id, project_id, expires_at)
    WHERE revoked_at IS NULL;
