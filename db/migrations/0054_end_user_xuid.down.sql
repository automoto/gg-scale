DROP INDEX IF EXISTS end_users_xuid_uniq;
ALTER TABLE end_users DROP COLUMN IF EXISTS xuid;
