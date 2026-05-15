DROP INDEX IF EXISTS end_users_disabled_idx;
ALTER TABLE end_users DROP COLUMN disabled_at;
