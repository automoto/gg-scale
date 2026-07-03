ALTER TABLE end_users DROP COLUMN IF EXISTS session_epoch;
DROP TABLE IF EXISTS tenant_player_bans;
ALTER TABLE player_accounts
    DROP COLUMN IF EXISTS secondary_remote_addr,
    DROP COLUMN IF EXISTS primary_remote_addr;
