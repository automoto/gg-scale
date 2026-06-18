CREATE INDEX end_users_project_email_idx
    ON end_users (project_id, email)
    WHERE deleted_at IS NULL;
