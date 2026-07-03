-- Rename the per-project player identity from "end user" to "player".
--
-- The per-project rows (formerly end_users) are players; the table becomes
-- project_players to disambiguate from the platform-global player_accounts
-- (unchanged) and from dashboard_users (operators, unchanged). No live data,
-- so ALTER … RENAME is transactional and lossless. Renames cover tables,
-- columns, indexes, constraints, sequences, RLS policies, and the SECURITY
-- DEFINER helpers.

-- Tables.
ALTER TABLE end_users RENAME TO project_players;
ALTER TABLE end_user_invitations RENAME TO player_invitations;

-- Player-id columns.
ALTER TABLE sessions            RENAME COLUMN end_user_id TO player_id;
ALTER TABLE matchmaking_tickets RENAME COLUMN end_user_id TO player_id;
ALTER TABLE presence            RENAME COLUMN end_user_id TO player_id;
ALTER TABLE game_session_peer   RENAME COLUMN end_user_id TO player_id;
ALTER TABLE leaderboard_entries RENAME COLUMN end_user_id TO player_id;
ALTER TABLE game_invite         RENAME COLUMN from_user_id TO from_player_id;
ALTER TABLE game_invite         RENAME COLUMN to_user_id   TO to_player_id;
ALTER TABLE game_session        RENAME COLUMN host_user_id TO host_player_id;

-- Sequences.
ALTER SEQUENCE end_users_id_seq            RENAME TO project_players_id_seq;
ALTER SEQUENCE end_user_invitations_id_seq RENAME TO player_invitations_id_seq;

-- project_players indexes.
ALTER INDEX end_users_pkey                          RENAME TO project_players_pkey;
ALTER INDEX end_users_disabled_idx                  RENAME TO project_players_disabled_idx;
ALTER INDEX end_users_email_uniq                    RENAME TO project_players_email_uniq;
ALTER INDEX end_users_email_verification_code_idx   RENAME TO project_players_email_verification_code_idx;
ALTER INDEX end_users_external_uniq                 RENAME TO project_players_external_uniq;
ALTER INDEX end_users_player_account_id_idx         RENAME TO project_players_player_account_id_idx;
ALTER INDEX end_users_project_created_id_idx        RENAME TO project_players_project_created_id_idx;
ALTER INDEX end_users_project_email_idx             RENAME TO project_players_project_email_idx;
ALTER INDEX end_users_project_email_trgm_idx        RENAME TO project_players_project_email_trgm_idx;
ALTER INDEX end_users_tenant_id_idx                 RENAME TO project_players_tenant_id_idx;
ALTER INDEX end_users_xuid_uniq                     RENAME TO project_players_xuid_uniq;

-- player_invitations indexes.
ALTER INDEX end_user_invitations_pkey               RENAME TO player_invitations_pkey;
ALTER INDEX end_user_invitations_code_lookup_idx    RENAME TO player_invitations_code_lookup_idx;
ALTER INDEX end_user_invitations_open_uq            RENAME TO player_invitations_open_uq;
ALTER INDEX end_user_invitations_tenant_idx         RENAME TO player_invitations_tenant_idx;

-- Indexes on renamed player-id columns.
ALTER INDEX sessions_end_user_id_idx    RENAME TO sessions_player_id_idx;
ALTER INDEX game_invite_to_user_idx     RENAME TO game_invite_to_player_idx;
ALTER INDEX leaderboard_entries_user_idx RENAME TO leaderboard_entries_player_idx;

-- Foreign-key constraints.
ALTER TABLE project_players    RENAME CONSTRAINT end_users_player_account_id_fkey TO project_players_player_account_id_fkey;
ALTER TABLE project_players    RENAME CONSTRAINT end_users_project_id_fkey        TO project_players_project_id_fkey;
ALTER TABLE project_players    RENAME CONSTRAINT end_users_tenant_id_fkey         TO project_players_tenant_id_fkey;
ALTER TABLE player_invitations RENAME CONSTRAINT end_user_invitations_invited_by_user_id_fkey TO player_invitations_invited_by_user_id_fkey;
ALTER TABLE player_invitations RENAME CONSTRAINT end_user_invitations_project_id_fkey         TO player_invitations_project_id_fkey;
ALTER TABLE player_invitations RENAME CONSTRAINT end_user_invitations_tenant_id_fkey          TO player_invitations_tenant_id_fkey;
ALTER TABLE game_invite        RENAME CONSTRAINT game_invite_from_user_id_fkey    TO game_invite_from_player_id_fkey;
ALTER TABLE game_invite        RENAME CONSTRAINT game_invite_to_user_id_fkey      TO game_invite_to_player_id_fkey;
ALTER TABLE game_session       RENAME CONSTRAINT game_session_host_user_id_fkey   TO game_session_host_player_id_fkey;
ALTER TABLE game_session_peer  RENAME CONSTRAINT game_session_peer_end_user_id_fkey TO game_session_peer_player_id_fkey;
ALTER TABLE leaderboard_entries RENAME CONSTRAINT leaderboard_entries_end_user_id_fkey TO leaderboard_entries_player_id_fkey;
ALTER TABLE matchmaking_tickets RENAME CONSTRAINT matchmaking_tickets_end_user_id_fkey TO matchmaking_tickets_player_id_fkey;
ALTER TABLE presence           RENAME CONSTRAINT presence_end_user_id_fkey        TO presence_player_id_fkey;
ALTER TABLE sessions           RENAME CONSTRAINT sessions_end_user_id_fkey        TO sessions_player_id_fkey;

-- RLS policies.
ALTER POLICY end_users_isolation            ON project_players    RENAME TO project_players_isolation;
ALTER POLICY end_user_invitations_isolation ON player_invitations RENAME TO player_invitations_isolation;

-- SECURITY DEFINER helpers. The tenant resolver is renamed (arg + body);
-- the two cross-tenant readers keep their names but their bodies and the
-- linked-projects output column follow the new table/column names.
DROP FUNCTION player_end_user_tenant(BIGINT);

CREATE FUNCTION project_player_tenant(p_player_id BIGINT)
RETURNS TABLE (
    tenant_id BIGINT
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT tenant_id
    FROM project_players
    WHERE id = p_player_id
      AND deleted_at IS NULL;
$$;

REVOKE ALL ON FUNCTION project_player_tenant(BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION project_player_tenant(BIGINT) TO ggscale_app;

DROP FUNCTION player_account_linked_projects(UUID);

CREATE FUNCTION player_account_linked_projects(p_account_id UUID)
RETURNS TABLE (
    player_id    BIGINT,
    tenant_id    BIGINT,
    project_id   BIGINT,
    project_name TEXT,
    external_id  TEXT,
    linked_at    TIMESTAMPTZ
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT e.id, e.tenant_id, e.project_id, p.name::text, e.external_id, e.created_at
    FROM project_players e
    JOIN projects p ON p.id = e.project_id
    WHERE e.player_account_id = p_account_id
      AND e.deleted_at IS NULL
    ORDER BY e.created_at;
$$;

REVOKE ALL ON FUNCTION player_account_linked_projects(UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION player_account_linked_projects(UUID) TO ggscale_app;

CREATE OR REPLACE FUNCTION player_invite_lookup(p_code_hash BYTEA)
RETURNS TABLE (
    id          BIGINT,
    tenant_id   BIGINT,
    project_id  BIGINT,
    email       CITEXT,
    expires_at  TIMESTAMPTZ,
    project_name TEXT
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT
        i.id,
        i.tenant_id,
        i.project_id,
        i.email,
        i.expires_at,
        p.name
    FROM player_invitations i
    JOIN projects p ON p.id = i.project_id
    WHERE i.code_hash = p_code_hash
      AND i.accepted_at IS NULL
      AND i.revoked_at IS NULL;
$$;
