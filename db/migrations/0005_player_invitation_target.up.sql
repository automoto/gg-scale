-- Admin "link player" invites target an existing project_players row so that
-- acceptance binds the proven email + account onto that row instead of creating
-- a new one. Nullable: a plain (non-targeted) email invite leaves it NULL and
-- keeps the existing find-or-create-by-email behavior.
ALTER TABLE public.player_invitations
    ADD COLUMN project_player_id BIGINT
        REFERENCES public.project_players(id) ON DELETE CASCADE;

-- Look up open invitations by their target player (pending-badge + resend paths).
CREATE INDEX player_invitations_target_idx
    ON public.player_invitations (project_player_id)
    WHERE project_player_id IS NOT NULL
      AND accepted_at IS NULL
      AND revoked_at IS NULL;

-- Surface project_player_id through the SECURITY DEFINER accept-lookup so the
-- invite-accept handler can bind onto the target row. RETURNS TABLE signature
-- changes require DROP + CREATE (CREATE OR REPLACE cannot alter output columns).
DROP FUNCTION public.player_invite_lookup(bytea);

CREATE FUNCTION public.player_invite_lookup(p_code_hash bytea)
    RETURNS TABLE(id bigint, tenant_id bigint, project_id bigint, email public.citext, expires_at timestamp with time zone, project_name text, project_player_id bigint)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
    SELECT
        i.id,
        i.tenant_id,
        i.project_id,
        i.email,
        i.expires_at,
        p.name,
        i.project_player_id
    FROM player_invitations i
    JOIN projects p ON p.id = i.project_id
    WHERE i.code_hash = p_code_hash
      AND i.accepted_at IS NULL
      AND i.revoked_at IS NULL;
$$;

REVOKE ALL ON FUNCTION public.player_invite_lookup(bytea) FROM PUBLIC;
GRANT ALL ON FUNCTION public.player_invite_lookup(bytea) TO ggscale_app;
