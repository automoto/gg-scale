-- Reverse the player rename: project_players → end_users, etc.

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

DROP FUNCTION player_account_linked_projects(UUID);

CREATE FUNCTION player_account_linked_projects(p_account_id UUID)
RETURNS TABLE (
    end_user_id BIGINT,
    tenant_id   BIGINT,
    project_id  BIGINT,
    project_name TEXT,
    external_id TEXT,
    linked_at   TIMESTAMPTZ
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

DROP FUNCTION project_player_tenant(BIGINT);

CREATE FUNCTION player_end_user_tenant(p_end_user_id BIGINT)
RETURNS TABLE (
    tenant_id BIGINT
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT tenant_id
    FROM project_players
    WHERE id = p_end_user_id
      AND deleted_at IS NULL;
$$;

REVOKE ALL ON FUNCTION player_end_user_tenant(BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION player_end_user_tenant(BIGINT) TO ggscale_app;

-- RLS policies.
ALTER POLICY project_players_isolation    ON project_players    RENAME TO end_users_isolation;
ALTER POLICY player_invitations_isolation ON player_invitations RENAME TO end_user_invitations_isolation;

-- Foreign-key constraints.
ALTER TABLE project_players    RENAME CONSTRAINT project_players_player_account_id_fkey TO end_users_player_account_id_fkey;
ALTER TABLE project_players    RENAME CONSTRAINT project_players_project_id_fkey        TO end_users_project_id_fkey;
ALTER TABLE project_players    RENAME CONSTRAINT project_players_tenant_id_fkey         TO end_users_tenant_id_fkey;
ALTER TABLE player_invitations RENAME CONSTRAINT player_invitations_invited_by_user_id_fkey TO end_user_invitations_invited_by_user_id_fkey;
ALTER TABLE player_invitations RENAME CONSTRAINT player_invitations_project_id_fkey         TO end_user_invitations_project_id_fkey;
ALTER TABLE player_invitations RENAME CONSTRAINT player_invitations_tenant_id_fkey          TO end_user_invitations_tenant_id_fkey;
ALTER TABLE game_invite        RENAME CONSTRAINT game_invite_from_player_id_fkey  TO game_invite_from_user_id_fkey;
ALTER TABLE game_invite        RENAME CONSTRAINT game_invite_to_player_id_fkey    TO game_invite_to_user_id_fkey;
ALTER TABLE game_session       RENAME CONSTRAINT game_session_host_player_id_fkey TO game_session_host_user_id_fkey;
ALTER TABLE game_session_peer  RENAME CONSTRAINT game_session_peer_player_id_fkey TO game_session_peer_end_user_id_fkey;
ALTER TABLE leaderboard_entries RENAME CONSTRAINT leaderboard_entries_player_id_fkey TO leaderboard_entries_end_user_id_fkey;
ALTER TABLE matchmaking_tickets RENAME CONSTRAINT matchmaking_tickets_player_id_fkey TO matchmaking_tickets_end_user_id_fkey;
ALTER TABLE presence           RENAME CONSTRAINT presence_player_id_fkey          TO presence_end_user_id_fkey;
ALTER TABLE sessions           RENAME CONSTRAINT sessions_player_id_fkey          TO sessions_end_user_id_fkey;

-- Indexes on player-id columns.
ALTER INDEX sessions_player_id_idx        RENAME TO sessions_end_user_id_idx;
ALTER INDEX game_invite_to_player_idx     RENAME TO game_invite_to_user_idx;
ALTER INDEX leaderboard_entries_player_idx RENAME TO leaderboard_entries_user_idx;

-- player_invitations indexes.
ALTER INDEX player_invitations_pkey            RENAME TO end_user_invitations_pkey;
ALTER INDEX player_invitations_code_lookup_idx RENAME TO end_user_invitations_code_lookup_idx;
ALTER INDEX player_invitations_open_uq         RENAME TO end_user_invitations_open_uq;
ALTER INDEX player_invitations_tenant_idx      RENAME TO end_user_invitations_tenant_idx;

-- project_players indexes.
ALTER INDEX project_players_pkey                        RENAME TO end_users_pkey;
ALTER INDEX project_players_disabled_idx                RENAME TO end_users_disabled_idx;
ALTER INDEX project_players_email_uniq                  RENAME TO end_users_email_uniq;
ALTER INDEX project_players_email_verification_code_idx RENAME TO end_users_email_verification_code_idx;
ALTER INDEX project_players_external_uniq               RENAME TO end_users_external_uniq;
ALTER INDEX project_players_player_account_id_idx       RENAME TO end_users_player_account_id_idx;
ALTER INDEX project_players_project_created_id_idx      RENAME TO end_users_project_created_id_idx;
ALTER INDEX project_players_project_email_idx           RENAME TO end_users_project_email_idx;
ALTER INDEX project_players_project_email_trgm_idx      RENAME TO end_users_project_email_trgm_idx;
ALTER INDEX project_players_tenant_id_idx               RENAME TO end_users_tenant_id_idx;
ALTER INDEX project_players_xuid_uniq                   RENAME TO end_users_xuid_uniq;

-- Sequences.
ALTER SEQUENCE project_players_id_seq    RENAME TO end_users_id_seq;
ALTER SEQUENCE player_invitations_id_seq RENAME TO end_user_invitations_id_seq;

-- Player-id columns.
ALTER TABLE sessions            RENAME COLUMN player_id TO end_user_id;
ALTER TABLE matchmaking_tickets RENAME COLUMN player_id TO end_user_id;
ALTER TABLE presence            RENAME COLUMN player_id TO end_user_id;
ALTER TABLE game_session_peer   RENAME COLUMN player_id TO end_user_id;
ALTER TABLE leaderboard_entries RENAME COLUMN player_id TO end_user_id;
ALTER TABLE game_invite         RENAME COLUMN from_player_id TO from_user_id;
ALTER TABLE game_invite         RENAME COLUMN to_player_id   TO to_user_id;
ALTER TABLE game_session        RENAME COLUMN host_player_id TO host_user_id;

-- Tables.
ALTER TABLE project_players    RENAME TO end_users;
ALTER TABLE player_invitations RENAME TO end_user_invitations;
