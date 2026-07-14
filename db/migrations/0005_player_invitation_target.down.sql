-- Restore the original accept-lookup (without project_player_id).
DROP FUNCTION public.player_invite_lookup(bytea);

CREATE FUNCTION public.player_invite_lookup(p_code_hash bytea)
    RETURNS TABLE(id bigint, tenant_id bigint, project_id bigint, email public.citext, expires_at timestamp with time zone, project_name text)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public'
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

REVOKE ALL ON FUNCTION public.player_invite_lookup(bytea) FROM PUBLIC;
GRANT ALL ON FUNCTION public.player_invite_lookup(bytea) TO ggscale_app;

DROP INDEX IF EXISTS player_invitations_target_idx;
ALTER TABLE public.player_invitations
    DROP COLUMN IF EXISTS project_player_id;
