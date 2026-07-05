ALTER TABLE player_accounts
    DROP COLUMN remote_addr_ip_lan,
    DROP COLUMN remote_addr_ip_public,
    DROP COLUMN remote_addr_dns,
    DROP COLUMN remote_addr_iroh,
    ADD COLUMN primary_remote_addr   TEXT,
    ADD COLUMN secondary_remote_addr TEXT;
