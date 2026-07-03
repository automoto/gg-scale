-- Remote addresses + tenant-wide player bans + end_user session revocation.

-- Remote addresses are opaque endpoint strings (IP, Tailscale name, iroh
-- EndpointID, …). We do NOT parse or guess the format — validation is
-- length + printable-non-control only, done in Go. Visibility is owner +
-- accepted friends + admins of linked projects; never public. Nullable.
ALTER TABLE player_accounts
    ADD COLUMN primary_remote_addr   TEXT,
    ADD COLUMN secondary_remote_addr TEXT;

-- Tenant-wide ban on a GLOBAL account. A tenant admin bans a player_account
-- across every project the tenant owns. Distinct from project-level disable
-- (end_users.disabled_at) and platform-level disable (player_accounts.disabled_at).
--
-- No RLS: enforcement runs in mixed transaction contexts (tenant Pool.Q,
-- account BootstrapQ), so every query filters by tenant_id explicitly rather
-- than relying on app.tenant_id — same approach as friend_edges.
CREATE TABLE tenant_player_bans (
    id                BIGSERIAL PRIMARY KEY,
    tenant_id         BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    player_account_id UUID   NOT NULL REFERENCES player_accounts(id) ON DELETE CASCADE,
    reason            TEXT,
    created_by        BIGINT REFERENCES dashboard_users(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, player_account_id)
);

CREATE INDEX tenant_player_bans_account_idx
    ON tenant_player_bans (player_account_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_player_bans TO ggscale_app;
GRANT USAGE, SELECT ON tenant_player_bans_id_seq TO ggscale_app;

-- Session epoch on end_users. Bumped on project disable and on tenant ban so a
-- stale (pre-ban) JWT is rejected at server-verify immediately, rather than
-- surviving the 15-minute access-token TTL. Embedded in the JWT as the
-- `sepoch` claim.
ALTER TABLE end_users
    ADD COLUMN session_epoch INTEGER NOT NULL DEFAULT 0;
