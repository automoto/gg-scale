DROP INDEX IF EXISTS dashboard_users_disabled_idx;
ALTER TABLE dashboard_users DROP COLUMN disabled_at;
