DROP FUNCTION IF EXISTS player_account_linked_projects(UUID);
DROP INDEX IF EXISTS end_users_player_account_id_idx;
ALTER TABLE end_users DROP COLUMN IF EXISTS player_account_id;
DROP TABLE IF EXISTS player_account_sessions;
DROP TABLE IF EXISTS player_accounts;
