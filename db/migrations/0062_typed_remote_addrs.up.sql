-- Typed remote addresses. Each column is one slot; the column identity
-- encodes the type and, for IPs, the server-derived LAN/public scope.
-- Values are validated and normalized in Go (internal/remoteaddr) and may
-- embed ":port" for the ip/dns slots. Visibility is unchanged: owner,
-- accepted friends, and admins of linked projects; never public.
ALTER TABLE player_accounts
    DROP COLUMN primary_remote_addr,
    DROP COLUMN secondary_remote_addr,
    ADD COLUMN remote_addr_ip_lan    TEXT,
    ADD COLUMN remote_addr_ip_public TEXT,
    ADD COLUMN remote_addr_dns       TEXT,
    ADD COLUMN remote_addr_iroh      TEXT;
